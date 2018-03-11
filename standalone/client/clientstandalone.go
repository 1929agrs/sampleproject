package main

import (
	"flag"
	"gofrugal/wstunnel/tunnel/client"
	"gopkg.in/inconshreveable/log15.v2"
	"os"
)

func main() {

	tunnelClientArg := &client.TunnelClientArg{}

	cliFlag := flag.NewFlagSet("client", flag.ExitOnError)
	cliFlag.StringVar(&tunnelClientArg.Token, "token", "", "token")
	cliFlag.StringVar(&tunnelClientArg.OrderNo, "order-no", "", "order number")
	cliFlag.StringVar(&tunnelClientArg.TunnelUrl, "tunnel-url", "", "tunnel url")
	cliFlag.StringVar(&tunnelClientArg.ServerPath, "server-url", "", "server url")

	cliFlag.Parse(os.Args[1:])

	err := client.NewWSTunnelClient(tunnelClientArg).Start()
	if err != nil {
		log15.Crit(err.Error())
		os.Exit(1)
	}
	<-make(chan struct{}, 0)
}
