package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/google/certificate-transparency-go/x509"
	"github.com/jcjones/ct-mapreduce/config"
	"github.com/jcjones/ct-mapreduce/engine"
	"github.com/jcjones/ct-mapreduce/storage"
	"github.com/mozilla/crlite/go"
	"github.com/mozilla/crlite/go/downloader"
	"github.com/mozilla/crlite/go/rootprogram"
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
)

const (
	permMode    = 0644
	permModeDir = 0755
)

var (
	inccadb      = flag.String("ccadb", "<path>", "input CCADB CSV path")
	crlpath      = flag.String("crlpath", "<path>", "root of folders of the form /<path>/<issuer> containing .crl files to be updated")
	revokedpath  = flag.String("revokedpath", "<path>", "output folder of revoked serial files of the form <issuer>")
	enrolledpath = flag.String("enrolledpath", "<path>", "output JSON file of issuers with their enrollment status")
	ctconfig     = config.NewCTConfig()

	illegalPath = regexp.MustCompile(`[^[:alnum:]\~\-\./]`)

	allowableAgeOfLocalCRL, _ = time.ParseDuration("336h")
)

type AggregateEngine struct {
	loadStorageDB storage.CertDatabase
	saveStorage   storage.StorageBackend
	remoteCache   storage.RemoteCache

	issuers *rootprogram.MozIssuers
	display *mpb.Progress
}

func makeFilenameFromUrl(crlUrl url.URL) string {
	filename := fmt.Sprintf("%s-%s", crlUrl.Hostname(), path.Base(crlUrl.Path))
	filename = strings.ToLower(filename)
	filename = illegalPath.ReplaceAllString(filename, "")

	hash := sha256.Sum256([]byte(crlUrl.String()))

	filename = strings.TrimSuffix(filename, ".crl")
	filename = fmt.Sprintf("%s-%s.crl", filename, hex.EncodeToString(hash[:8]))
	return filename
}

func (ae *AggregateEngine) findCrlWorker(wg *sync.WaitGroup, issuerChan <-chan storage.Issuer, quitChan <-chan struct{}, resultChan chan<- types.IssuerCrlMap, progBar *mpb.Bar) {
	defer wg.Done()

	lastTime := time.Now()

	issuerCrls := make(types.IssuerCrlMap)

	for issuer := range issuerChan {
		select {
		case <-quitChan:
			return
		default:
			meta := ae.loadStorageDB.GetIssuerMetadata(issuer)

			crls, prs := issuerCrls[issuer.ID()]
			if !prs {
				crls = make(map[string]bool)
			}

			crlSet := meta.CRLs()

			if len(crlSet) == 0 {
				if ae.issuers.IsIssuerInProgram(issuer) {
					issuerSubj, err := ae.issuers.GetSubjectForIssuer(issuer)
					if err != nil {
						glog.Warningf("No known CRLs and couldn't get subject for issuer=%s that is in the root program: %s",
							issuer.ID(), err)
					} else {
						glog.Infof("No known CRLs for issuer=%s (%s) in the root program. Not enrolling into CRLite.",
							issuer.ID(), issuerSubj)
					}
				}
			}

			for _, url := range crlSet {
				crls[url] = true
			}
			issuerCrls[issuer.ID()] = crls

			progBar.IncrBy(1, time.Since(lastTime))
			lastTime = time.Now()
		}
	}

	resultChan <- issuerCrls
}

func (ae *AggregateEngine) crlFetchWorker(wg *sync.WaitGroup, crlsChan <-chan types.IssuerCrlUrls, quitChan <-chan struct{}, resultChan chan<- types.IssuerCrlPaths, progBar *mpb.Bar) {
	defer wg.Done()

	lastTime := time.Now()

	for tuple := range crlsChan {
		paths := make([]string, 0)

		for _, crlUrl := range tuple.Urls {
			select {
			case <-quitChan:
				return
			default:
			}

			filename := makeFilenameFromUrl(crlUrl)
			err := os.MkdirAll(filepath.Join(*crlpath, tuple.Issuer.ID()), permModeDir)
			if err != nil {
				glog.Warningf("Couldn't make directory: %s", err)
				continue
			}

			tmpPath := filepath.Join(*crlpath, tuple.Issuer.ID(), filename+".tmp")
			finalPath := filepath.Join(*crlpath, tuple.Issuer.ID(), filename)

			err = downloader.DownloadFileSync(ae.display, crlUrl, tmpPath, 3)
			if err != nil {
				glog.Warningf("[%s] Could not download %s to %s: %s", tuple.Issuer.ID(), crlUrl.String(), tmpPath, err)
			} else {
				// Validate the file and move it to the finalPath
				cert, err := ae.issuers.GetCertificateForIssuer(tuple.Issuer)
				if err != nil {
					glog.Fatalf("[%s] Could not find certificate for issuer: %s", tuple.Issuer.ID(), err)
				}
				_, err = processCRL(tmpPath, cert)
				if err != nil {
					glog.Warningf("[%s] Downloaded %s to %s but file didn't validate: %s", tuple.Issuer.ID(), crlUrl.String(), tmpPath, err)
				} else {
					err = os.Rename(tmpPath, finalPath)
					if err != nil {
						glog.Errorf("[%s] Couldn't rename %s to %s: %s", tuple.Issuer.ID(), tmpPath, finalPath, err)
					}
				}
			}

			// Ensure the final path is acceptable
			_, localDate, err := downloader.GetSizeAndDateOfFile(finalPath)
			if err != nil {
				glog.Errorf("[%s] Could not download, and no local file, will not be populating the revocations: %s", crlUrl.String(), err)
				continue
			}

			age := time.Now().Sub(localDate)

			if age > allowableAgeOfLocalCRL {
				glog.Errorf("[%s] Could not download, and out of date local file, will not be populating the revocations. Age: %s", crlUrl.String(), age.String())
				continue
			}

			paths = append(paths, finalPath)
		}

		resultChan <- types.IssuerCrlPaths{
			Issuer:   tuple.Issuer,
			CrlPaths: paths,
		}

		progBar.IncrBy(1, time.Since(lastTime))
		lastTime = time.Now()
	}
}

func processCRL(aPath string, aIssuerCert *x509.Certificate) ([]storage.Serial, error) {
	serials := make([]storage.Serial, 0, 1024*16)

	glog.V(1).Infof("[%s] Proesssing CRL", aPath)
	crlBytes, err := ioutil.ReadFile(aPath)
	if err != nil {
		return serials, fmt.Errorf("Error reading CRL, will not process revocations: %s", err)
	}

	crl, err := x509.ParseCRL(crlBytes)
	if err != nil {
		return serials, fmt.Errorf("Error parsing, will not process revocations: %s", err)
	}

	if err = aIssuerCert.CheckCRLSignature(crl); err != nil {
		return serials, fmt.Errorf("Invalid signature on CRL, will not process revocations: %s", err)
	}

	if crl.HasExpired(time.Now()) {
		glog.Warningf("[%s] CRL is expired, but proceeding anyway", aPath)
	}

	// Decode the raw DER serial numbers
	revokedList, err := types.DecodeRawTBSCertList(crl.TBSCertList.Raw)
	if err != nil {
		return serials, fmt.Errorf("CRL list couldn't be decoded: %s", err)
	}

	for _, ent := range revokedList.RevokedCertificates {
		serial := storage.NewSerialFromBytes(ent.SerialNumber.Bytes)
		serials = append(serials, serial)
	}

	return serials, nil
}

func (ae *AggregateEngine) aggregateCRLWorker(wg *sync.WaitGroup, workChan <-chan types.IssuerCrlPaths, quitChan <-chan struct{}, progBar *mpb.Bar) {
	defer wg.Done()

	ctx := context.Background()

	for tuple := range workChan {
		cycleTime := time.Now()

		issuerEnrolled := false

		cert, err := ae.issuers.GetCertificateForIssuer(tuple.Issuer)
		if err != nil {
			glog.Fatalf("[%s] Could not find certificate for issuer: %s", tuple.Issuer.ID(), err)
		}

		serialCount := 0
		serials := make([]storage.Serial, 0, 128*1024)

		for _, crlPath := range tuple.CrlPaths {
			select {
			case <-quitChan:
				return
			default:
				revokedSerials, err := processCRL(crlPath, cert)
				if err != nil {
					glog.Errorf("[%s] Failed to process: %s", crlPath, err)
					continue
				}

				revokedCount := len(revokedSerials)
				if revokedCount == 0 {
					continue
				}

				// Issuer is considered enrolled if at least one CRL processed successfully
				if !issuerEnrolled {
					issuerEnrolled = true
					ae.issuers.Enroll(tuple.Issuer)
				}

				if cap(serials) < revokedCount+serialCount {
					newSerials := make([]storage.Serial, 0, serialCount+revokedCount)
					copy(newSerials, serials)
					serials = newSerials
				}

				serials = append(serials, revokedSerials...)
				serialCount += revokedCount
			}
		}

		if issuerEnrolled {
			glog.Infof("[%s] Saving %d revoked serials", tuple.Issuer.ID(), serialCount)
			if err := ae.saveStorage.StoreKnownCertificateList(ctx, tuple.Issuer, serials); err != nil {
				glog.Fatalf("[%s] Could not save revoked certificates file: %s", tuple.Issuer.ID(), err)
			}
		} else {
			glog.Infof("Issuer %s not enrolled", tuple.Issuer.ID())
		}

		progBar.IncrBy(1, time.Since(cycleTime))
	}
}

func (ae *AggregateEngine) identifyCrlsByIssuer(sigChan <-chan os.Signal) types.IssuerCrlMap {
	var wg sync.WaitGroup

	glog.Infof("Listing issuers and their expiration dates...")
	issuerList, err := ae.loadStorageDB.GetIssuerAndDatesFromCache()
	if err != nil {
		glog.Fatal(err)
	}

	issuerChan := make(chan storage.Issuer, len(issuerList))

	var count int64
	for _, issuerObj := range issuerList {
		if !ae.issuers.IsIssuerInProgram(issuerObj.Issuer) {
			continue
		}

		select {
		case issuerChan <- issuerObj.Issuer:
			count = count + 1
		default:
			glog.Fatalf("Channel overflow. Aborting at %s", issuerObj.Issuer.ID())
		}
	}

	// Signal that was the last work
	close(issuerChan)

	// Exit signal, used by signals from the OS
	quitChan := make(chan struct{})

	progressBar := ae.display.AddBar(count,
		mpb.PrependDecorators(
			decor.Name("Identify CRLs"),
		),
		mpb.AppendDecorators(
			decor.Percentage(),
			decor.Name(""),
			decor.AverageETA(decor.ET_STYLE_GO, decor.WC{W: 14}),
			decor.CountersNoUnit("%d / %d", decor.WCSyncSpace),
		),
		mpb.BarRemoveOnComplete(),
	)

	resultChan := make(chan types.IssuerCrlMap, *ctconfig.NumThreads)

	// Start the workers
	for t := 0; t < *ctconfig.NumThreads; t++ {
		wg.Add(1)
		go ae.findCrlWorker(&wg, issuerChan, quitChan, resultChan, progressBar)
	}

	// Set up a notifier for the workers closing
	doneChan := make(chan bool)
	go func(wait *sync.WaitGroup) {
		wait.Wait()
		doneChan <- true
	}(&wg)

	select {
	case <-sigChan:
		glog.Infof("Signal caught, stopping threads at next opportunity.")
		quitChan <- struct{}{}
		return nil
	case <-doneChan:
		close(resultChan)
	}

	// Take all worker results and merge them into one JSON structure
	mergedCrls := make(types.IssuerCrlMap)
	for mapPart := range resultChan {
		mergedCrls.Merge(mapPart)
	}

	return mergedCrls
}

func (ae *AggregateEngine) downloadCRLs(issuerToUrls types.IssuerCrlMap, sigChan <-chan os.Signal) (<-chan types.IssuerCrlPaths, int64) {
	var wg sync.WaitGroup

	// Exit signal, used by signals from the OS
	quitChan := make(chan struct{})

	crlChan := make(chan types.IssuerCrlUrls, 16*1024*1024)
	var count int64
	for issuer, crlMap := range issuerToUrls {
		var urls []url.URL

		for iUrl := range crlMap {
			urlObj, err := url.Parse(strings.TrimSpace(iUrl))
			if err != nil {
				glog.Warningf("Ignoring URL %s: %s", iUrl, err)
				continue
			}
			urls = append(urls, *urlObj)
		}

		if len(urls) > 0 {
			crlChan <- types.IssuerCrlUrls{
				Issuer: storage.NewIssuerFromString(issuer),
				Urls:   urls,
			}
			count = count + 1
		}
	}
	close(crlChan)

	progressBar := ae.display.AddBar(count,
		mpb.PrependDecorators(
			decor.Name("Download CRLs"),
		),
		mpb.AppendDecorators(
			decor.Percentage(),
			decor.Name(""),
			decor.AverageETA(decor.ET_STYLE_GO, decor.WC{W: 14}),
			decor.CountersNoUnit("%d / %d", decor.WCSyncSpace),
		),
		mpb.BarRemoveOnComplete(),
	)

	resultChan := make(chan types.IssuerCrlPaths, count)

	// Start the workers
	for t := 0; t < *ctconfig.NumThreads; t++ {
		wg.Add(1)
		go ae.crlFetchWorker(&wg, crlChan, quitChan, resultChan, progressBar)
	}

	// Set up a notifier for the workers closing
	doneChan := make(chan bool)
	go func(wait *sync.WaitGroup) {
		wait.Wait()
		doneChan <- true
	}(&wg)

	select {
	case <-sigChan:
		glog.Infof("Signal caught, stopping threads at next opportunity.")
		quitChan <- struct{}{}
		return nil, 0
	case <-doneChan:
		close(resultChan)
	}

	return resultChan, count
}

func (ae *AggregateEngine) aggregateCRLs(count int64, crlPaths <-chan types.IssuerCrlPaths, sigChan <-chan os.Signal) {
	var wg sync.WaitGroup

	// Exit signal, used by signals from the OS
	quitChan := make(chan struct{})

	progressBar := ae.display.AddBar(count,
		mpb.PrependDecorators(
			decor.Name("Aggregate CRLs"),
		),
		mpb.AppendDecorators(
			decor.Percentage(),
			decor.Name(""),
			decor.AverageETA(decor.ET_STYLE_GO, decor.WC{W: 14}),
			decor.CountersNoUnit("%d / %d", decor.WCSyncSpace),
		),
		mpb.BarRemoveOnComplete(),
	)

	// Start the workers
	for t := 0; t < *ctconfig.NumThreads; t++ {
		wg.Add(1)
		go ae.aggregateCRLWorker(&wg, crlPaths, quitChan, progressBar)
	}

	// Set up a notifier for the workers closing
	doneChan := make(chan bool)
	go func(wait *sync.WaitGroup) {
		wait.Wait()
		doneChan <- true
	}(&wg)

	select {
	case <-sigChan:
		glog.Infof("Signal caught, stopping threads at next opportunity.")
		quitChan <- struct{}{}
		return
	case <-doneChan:
		glog.Infof("Completed.")
	}
}

func checkPathArg(strObj string, confOptionName string, ctconfig *config.CTConfig) {
	if strObj == "<path>" {
		glog.Errorf("Flag %s is not set", confOptionName)
		ctconfig.Usage()
		os.Exit(2)
	}
}

func main() {
	ctconfig.Init()
	ctx := context.Background()
	storageDB, remoteCache, _ := engine.GetConfiguredStorage(ctx, ctconfig)
	defer glog.Flush()

	checkPathArg(*revokedpath, "revokedpath", ctconfig)
	checkPathArg(*crlpath, "crlpath", ctconfig)
	checkPathArg(*enrolledpath, "enrolledpath", ctconfig)

	if err := os.MkdirAll(*revokedpath, permModeDir); err != nil {
		glog.Fatalf("Unable to make the revokedpath directory: %s", err)
	}
	if err := os.MkdirAll(*crlpath, permModeDir); err != nil {
		glog.Fatalf("Unable to make the CRL directory: %s", err)
	}

	refreshDur, err := time.ParseDuration(*ctconfig.OutputRefreshPeriod)
	if err != nil {
		glog.Fatal(err)
	}
	glog.Infof("Progress bar refresh rate is every %s.\n", refreshDur.String())

	engine.PrepareTelemetry("aggregate-crls", ctconfig)

	saveBackend := storage.NewLocalDiskBackend(permMode, *revokedpath)

	mozIssuers := rootprogram.NewMozillaIssuers()
	if *inccadb != "<path>" {
		err = mozIssuers.LoadFromDisk(*inccadb)
	} else {
		err = mozIssuers.Load()
	}

	if err != nil {
		glog.Fatalf("Unable to load the Mozilla issuers: %s", err)
	}

	// Handle signals from the OS
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sigChan)

	display := mpb.NewWithContext(ctx,
		mpb.WithRefreshRate(refreshDur),
	)

	ae := AggregateEngine{
		loadStorageDB: storageDB,
		saveStorage:   saveBackend,
		remoteCache:   remoteCache,
		issuers:       mozIssuers,
		display:       display,
	}

	mergedCrls := ae.identifyCrlsByIssuer(sigChan)
	if mergedCrls == nil {
		return
	}

	crlPaths, count := ae.downloadCRLs(mergedCrls, sigChan)

	ae.aggregateCRLs(count, crlPaths, sigChan)
	if err = mozIssuers.SaveIssuersList(*enrolledpath); err != nil {
		glog.Fatalf("Unable to save the crlite-informed intermediate issuers to %s: %s", *enrolledpath, err)
	}
	glog.Infof("Saved crlite-informed intermediate issuers to %s", *enrolledpath)
}
