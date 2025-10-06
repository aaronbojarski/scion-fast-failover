package main

import (
	"context"
	"log"
	"net"
	"net/netip"

	"github.com/scionproto/scion/pkg/addr"
	"github.com/scionproto/scion/pkg/daemon"
	"github.com/scionproto/scion/pkg/snet"
)

func runClient(daemonAddr string, localAddr, remoteAddr snet.UDPAddr) {
	var data string

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
			Path: sp.Dataplane(),
			Payload: snet.UDPPayload{
				SrcPort: uint16(localAddr.Host.Port),
				DstPort: uint16(remoteAddr.Host.Port),
				Payload: []byte(data),
			},
		},
	}

	nextHop := sp.UnderlayNextHop()
	if nextHop == nil && remoteAddr.IA == localAddr.IA {
		nextHop = remoteAddr.Host
	}

	err = pkt.Serialize()
	if err != nil {
		log.Fatalf("Failed to serialize SCION packet: %v", err)
	}

	_, err = conn.WriteTo(pkt.Bytes, nextHop)
	if err != nil {
		log.Fatalf("Failed to write packet: %v", err)
	}

	pkt.Prepare()
	n, _, err := conn.ReadFrom(pkt.Bytes)
	if err != nil {
		log.Fatalf("Failed to read packet: %v", err)
	}
	pkt.Bytes = pkt.Bytes[:n]

	err = pkt.Decode()
	if err != nil {
		log.Fatalf("Failed to decode packet: %v", err)
	}

	pld, ok := pkt.Payload.(snet.UDPPayload)
	if !ok {
		log.Fatal("Failed to read packet payload")
	}

	log.Printf("Received data: \"%s\"", string(pld.Payload))
}
