// SCION time service

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/scionproto/scion/pkg/snet"
)

func exitWithUsage() {
	fmt.Println("<usage>")
	os.Exit(1)
}

func main() {
	var (
		daemonAddr string
		localAddr  snet.UDPAddr
		remoteAddr snet.UDPAddr
	)

	serverFlags := flag.NewFlagSet("server", flag.ExitOnError)
	clientFlags := flag.NewFlagSet("client", flag.ExitOnError)

	serverFlags.Var(&localAddr, "local", "Local address")

	clientFlags.StringVar(&daemonAddr, "daemon", "", "Daemon address")
	clientFlags.Var(&localAddr, "local", "Local address")
	clientFlags.Var(&remoteAddr, "remote", "Remote address")

	if len(os.Args) < 2 {
		exitWithUsage()
	}

	switch os.Args[1] {
	case serverFlags.Name():
		err := serverFlags.Parse(os.Args[2:])
		if err != nil || serverFlags.NArg() != 0 {
			exitWithUsage()
		}
		runServer(localAddr)
	case clientFlags.Name():
		err := clientFlags.Parse(os.Args[2:])
		if err != nil || clientFlags.NArg() != 0 {
			exitWithUsage()
		}
		runClient(daemonAddr, localAddr, remoteAddr)
	default:
		exitWithUsage()
	}
}
