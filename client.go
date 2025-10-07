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

	var wg sync.WaitGroup
	wg.Add(1)

	var currentPath int = 0
	sendingTimes := make([]time.Time, 10000)
	received := make([]bool, 10000)
	receivingTimes := make([]time.Time, 10000)

	// Send Packet
	go sendPackets(conn, localAddr, remoteAddr, ps, &wg, &currentPath, sendingTimes)

	// Receive Packet
	go receivePackets(conn, ps, &currentPath, received, receivingTimes)

	wg.Wait()

	totalReceived := 0
	rttSum := time.Duration(0)
	lastReceived := -1
	reconnectTimes := make([]time.Duration, 0, 100)
	for i := range 10000 {
		if received[i] {
			totalReceived += 1
			rttSum += receivingTimes[i].Sub(sendingTimes[i])
			if !(lastReceived == i-1) && lastReceived != -1 {
				reconnectTimes = append(reconnectTimes, receivingTimes[i].Sub(receivingTimes[lastReceived]))
			}
			lastReceived = i
		}
	}

	log.Print("Stats:")
	log.Printf("Total Received: %v", totalReceived)
	log.Printf("RTT Average: %v", rttSum/time.Duration(totalReceived))
	log.Printf("ReconnectTimes: %v", reconnectTimes)
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

func receivePackets(conn *net.UDPConn, paths []snet.Path, currentPath *int, received []bool, receivingTimes []time.Time) {
	defer conn.Close()

	failedPaths := make([]bool, len(paths))
	for {
		// check if we have healthy paths left and set one
		if failedPaths[*currentPath] {
			hasHealthy := false
			for i := *currentPath; i < len(paths); i++ {
				if !failedPaths[i] {
					log.Printf("Switching to Path: %d", i)
					*currentPath = i
					hasHealthy = true
					break
				}
			}
			if !hasHealthy {
				log.Fatal("No More Healthy Paths")
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
			id := binary.LittleEndian.Uint32(pld.Payload)
			received[id] = true
			receivingTimes[id] = time.Now()
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

func sendPackets(conn *net.UDPConn, localAddr, remoteAddr snet.UDPAddr, paths []snet.Path, wg *sync.WaitGroup, currentPath *int, sendingTimes []time.Time) {
	defer wg.Done()
	data := make([]byte, 4)
	for i := range uint32(1000) {
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
		sendingTimes[i] = time.Now()
		time.Sleep(time.Duration(interval) * time.Millisecond)
	}
	// wait for response of last packets
	time.Sleep(2 * time.Second)
}
