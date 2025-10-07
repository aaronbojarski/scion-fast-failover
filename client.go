package main

import (
	"context"
	"encoding/binary"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/scionproto/scion/pkg/addr"
	"github.com/scionproto/scion/pkg/daemon"
	"github.com/scionproto/scion/pkg/slayers"
	"github.com/scionproto/scion/pkg/snet"
	"github.com/scionproto/scion/pkg/snet/path"
)

// Main Idea:
// Have 1 goroutine send packets all ..ms for fixed number of messages / time.
// Second goroutine receives responses, logs them and changes the path if necessary.

const (
	interval int = 10
)

func runClient(daemonAddr string, localAddr, remoteAddr snet.UDPAddr) {
	ctx := context.Background()

	dc, err := daemon.NewService(daemonAddr).Connect(ctx)
	if err != nil {
		log.Fatalf("Failed to create SCION daemon connector: %v", err)
	}

	ps, err := dc.Paths(ctx, remoteAddr.IA, localAddr.IA, daemon.PathReqFlags{Refresh: true})
	if err != nil {
		log.Fatalf("Failed to lookup paths: %v", err)
	}

	if len(ps) == 0 {
		log.Fatalf("No paths to %v available", remoteAddr.IA)
	}

	log.Printf("Available paths to %v:", remoteAddr.IA)
	for _, p := range ps {
		log.Printf("\t%v", p)
	}

	sp := ps[0]

	log.Printf("Selected path to %v:", remoteAddr.IA)
	log.Printf("\t%v", sp)

	conn, err := net.ListenUDP("udp", localAddr.Host)
	if err != nil {
		log.Fatalf("Failed to bind UDP connection: %v", err)
	}
	defer conn.Close()

	var wg sync.WaitGroup
	wg.Add(1)

	var currentPath int = 0

	// Send Packet
	go sendPackets(conn, localAddr, remoteAddr, ps, &wg, &currentPath)

	// Receive Packet
	go receivePackets(conn, ps, &currentPath)

	wg.Wait()
}

func contains(path path.Path, ia addr.IA, interfaceId uint64) bool {
	ifaces := path.Meta.Interfaces
	intf := ifaces[0]
	if intf.IA == ia && uint64(intf.ID) == interfaceId {
		return true
	}
	for i := 1; i < len(ifaces)-1; i += 2 {
		inIntf := ifaces[i]
		outIntf := ifaces[i+1]
		if inIntf.IA == ia && uint64(inIntf.ID) == interfaceId {
			return true
		}
		if outIntf.IA == ia && uint64(outIntf.ID) == interfaceId {
			return true
		}
	}
	intf = ifaces[len(ifaces)-1]
	if intf.IA == ia && uint64(intf.ID) == interfaceId {
		return true
	}
	return false
}

func receivePackets(conn *net.UDPConn, paths []snet.Path, currentPath *int) {
	failedPaths := make([]bool, len(paths))
	for {
		// check if we have healthy paths left and set one
		if failedPaths[*currentPath] {
			for i := *currentPath; i < len(paths); i++ {
				if !failedPaths[i] {
					log.Printf("Switching to Path: %d", i)
					*currentPath = i
					break
				}
			}
		}
		pkt := &snet.Packet{}
		pkt.Prepare()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, _, err := conn.ReadFrom(pkt.Bytes)
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Timeout() {
				// Timeout. Mark path as failed.
				failedPaths[*currentPath] = true
			} else {
				log.Fatalf("Failed to read packet: %v", err)
			}
		}
		log.Print("Receiving New Message.")
		pkt.Bytes = pkt.Bytes[:n]

		err = pkt.Decode()
		if err != nil {
			log.Fatalf("Failed to decode packet: %v", err)
		}

		pld, ok := pkt.Payload.(snet.UDPPayload)
		if ok {
			log.Printf("Received data: \"%d\"", binary.LittleEndian.Uint32(pld.Payload))
			continue
		}
		scmp, ok := pkt.Payload.(snet.SCMPPayload)
		if !ok {
			log.Fatal("Not an SCMP packet.")
		}
		switch scmp.Type() {
		case slayers.SCMPTypeExternalInterfaceDown:
			msg := pkt.Payload.(snet.SCMPExternalInterfaceDown)
			for i := 0; i < len(paths); i++ {
				if contains(paths[i].(path.Path), msg.IA, msg.Interface) {
					failedPaths[i] = true
				}
			}
		case slayers.SCMPTypeInternalConnectivityDown:
			log.Fatalf("SCMPInternalConnectivityDown: Path: %v", pkt.Path)

		default:
			log.Fatalf("SCMP: Type: %v", scmp.Type())
			continue
		}
	}
}

func sendPackets(conn *net.UDPConn, localAddr, remoteAddr snet.UDPAddr, paths []snet.Path, wg *sync.WaitGroup, currentPath *int) {
	defer wg.Done()
	data := make([]byte, 4)
	for i := range uint32(10000) {
		log.Printf("Sending New Message. %d over path %d", i, *currentPath)

		path := paths[*currentPath]
		binary.LittleEndian.PutUint32(data, i)
		srcAddr, ok := netip.AddrFromSlice(localAddr.Host.IP)
		if !ok {
			log.Fatal("Unexpected address type")
		}
		srcAddr = srcAddr.Unmap()
		dstAddr, ok := netip.AddrFromSlice(remoteAddr.Host.IP)
		if !ok {
			log.Fatal("Unexpected address type")
		}
		dstAddr = dstAddr.Unmap()

		pkt := &snet.Packet{
			PacketInfo: snet.PacketInfo{
				Source: snet.SCIONAddress{
					IA:   localAddr.IA,
					Host: addr.HostIP(srcAddr),
				},
				Destination: snet.SCIONAddress{
					IA:   remoteAddr.IA,
					Host: addr.HostIP(dstAddr),
				},
				Path: path.Dataplane(),
				Payload: snet.UDPPayload{
					SrcPort: uint16(localAddr.Host.Port),
					DstPort: uint16(remoteAddr.Host.Port),
					Payload: data,
				},
			},
		}

		nextHop := path.UnderlayNextHop()
		if nextHop == nil && remoteAddr.IA == localAddr.IA {
			nextHop = remoteAddr.Host
		}

		err := pkt.Serialize()
		if err != nil {
			log.Fatalf("Failed to serialize SCION packet: %v", err)
		}

		_, err = conn.WriteTo(pkt.Bytes, nextHop)
		if err != nil {
			log.Fatalf("Failed to write packet: %v", err)
		}
		time.Sleep(time.Duration(interval) * time.Millisecond)
	}
	// wait for response of last packets
	time.Sleep(2 * time.Second)
}
