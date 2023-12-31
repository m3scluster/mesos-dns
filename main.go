package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/AVENTER-UG/mesos-dns/detect"
	"github.com/AVENTER-UG/mesos-dns/logging"
	"github.com/AVENTER-UG/mesos-dns/records"
	"github.com/AVENTER-UG/mesos-dns/resolver"
	"github.com/AVENTER-UG/mesos-dns/util"
	"github.com/mesos/mesos-go/api/v0/detector"
)

func main() {
	util.PanicHandlers = append(util.PanicHandlers, func(_ interface{}) {
		// by default the handler already logs the panic
		os.Exit(1)
	})

	var versionFlag bool

	// parse flags
	cjson := flag.String("config", "config.json", "path to config file (json)")
	flag.BoolVar(&versionFlag, "version", false, "output the version")
	flag.Parse()

	// -version
	if versionFlag {
		fmt.Println(Version)
		os.Exit(0)
	}

	// initialize logging
	logging.SetupLogs()
	initAuth()

	// initialize resolver
	config := records.SetConfig(*cjson)
	res := resolver.New(Version, config)
	errch := make(chan error)

	// launch DNS server
	if config.DNSOn {
		go func() {
			logging.Verbose.Println("waiting for a reload before serving DNS")
			<-res.Ready()
			logging.Verbose.Println("reload occurred, serving DNS")
			errch <- <-res.LaunchDNS()
		}()
	}

	// launch HTTP server
	if config.HTTPOn {
		go func() {
			logging.Verbose.Println("waiting for a reload before serving HTTP")
			<-res.Ready()
			logging.Verbose.Println("reload occurred, serving HTTP")
			errch <- <-res.LaunchHTTP()
		}()
	}

	changed := detectMasters(config.Zk, config.Masters)
	reload := time.NewTicker(time.Second * time.Duration(config.RefreshSeconds))
	zkTimeout := time.Second * time.Duration(config.ZkDetectionTimeout)
	timeout := time.AfterFunc(zkTimeout, func() {
		if zkTimeout > 0 {
			errch <- fmt.Errorf("master detection timed out after %s", zkTimeout)
		}
	})

	defer reload.Stop()
	defer util.HandleCrash()
	for {
		select {
		case <-reload.C:
			res.Reload()
		case masters := <-changed:
			if len(masters) == 0 || masters[0] == "" { // no leader
				timeout.Reset(zkTimeout)
			} else {
				timeout.Stop()
			}
			logging.Verbose.Printf("new masters detected: %v", masters)
			res.SetMasters(masters)
			res.Reload()
		case err := <-errch:
			logging.Error.Fatal(err)
		}
	}
}

func detectMasters(zk string, masters []string) <-chan []string {
	changed := make(chan []string, 1)
	if zk != "" {
		logging.Verbose.Println("Starting master detector for ZK ", zk)
		if md, err := detector.New(zk); err != nil {
			log.Fatalf("failed to create master detector: %v", err)
		} else if err := md.Detect(detect.NewMasters(masters, changed)); err != nil {
			log.Fatalf("failed to initialize master detector: %v", err)
		}
	} else {
		changed <- masters
	}
	return changed
}
