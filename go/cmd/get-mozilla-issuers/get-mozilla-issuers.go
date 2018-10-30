package main

import (
	"encoding/json"
	"flag"
	"os"

	"github.com/golang/glog"
	"github.com/mozilla/crlite/go/rootprogram"
)

var (
	outfile = flag.String("out", "<stdout>", "output json dictionary of issuers")
	inccadb = flag.String("ccadb", "<path>", "input CCADB CSV path")
)

func main() {
	flag.Parse()

	var err error

	mozIssuers := rootprogram.NewMozillaIssuers()

	if *inccadb != "<path>" {
		err = mozIssuers.LoadFromDisk(*inccadb)
	} else {
		err = mozIssuers.Load()
	}

	if err != nil {
		glog.Fatal(err)
	}

	if *outfile == "<stdout>" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		if err = enc.Encode(mozIssuers.GetIssuers()); err != nil {
			glog.Fatal(err)
		}
		return
	}

	f, err := os.OpenFile(*outfile, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		glog.Fatal(err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	if err = enc.Encode(mozIssuers.GetIssuers()); err != nil {
		glog.Fatal(err)
	}
}