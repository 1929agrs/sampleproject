package main

import (
	"os"
	"gofrugal/wstunnel/tunnel/server"
	"gofrugal/wstunnel/tunnel/util"
	"github.com/inconshreveable/log15"
	"fmt"
)

func main() {

	helpers.SetVV(VV)
	// Config logging handler
	helpers.RegisterLogger(false, "logs/wstunnel.log", "")
	log15.Info(fmt.Sprintf("app version : %s", helpers.VV))

	server.NewWSTunnelServer(os.Args[1:]).Start(nil)
	<-make(chan struct{}, 0)
}
