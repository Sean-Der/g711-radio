package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
)

func main() {
	pcapPath := flag.String("pcap", "g711.pcapng", "pcapng file to replay")
	targetAddr := flag.String("addr", "127.0.0.1:2250", "UDP destination for replayed payloads")
	flag.Parse()

	packets, err := loadUDPPackets(*pcapPath)
	if err != nil {
		log.Fatal(err)
	}
	if len(packets) == 0 {
		log.Fatalf("no UDP packets found in %s", *pcapPath)
	}

	conn, err := net.Dial("udp", *targetAddr)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	log.Printf(
		"replaying %d UDP packet(s) from %s to %s, looping forever with original timing",
		len(packets),
		*pcapPath,
		*targetAddr,
	)

	totalPackets := 0

	for {
		if err := replayPackets(conn, packets); err != nil {
			log.Fatal(err)
		}
		totalPackets += len(packets)
		log.Printf("completed replay loop, sent %d packet(s) total", totalPackets)
	}
}

type udpPacket struct {
	timestamp time.Time
	payload   []byte
}

func loadUDPPackets(path string) ([]udpPacket, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader, err := pcapgo.NewNgReader(file, pcapgo.DefaultNgReaderOptions)
	if err != nil {
		return nil, fmt.Errorf("open pcapng: %w", err)
	}

	var packets []udpPacket

	for {
		data, ci, err := reader.ReadPacketData()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read packet: %w", err)
		}

		packet := gopacket.NewPacket(data, reader.LinkType(), gopacket.Default)
		udpLayer := packet.Layer(layers.LayerTypeUDP)
		if udpLayer == nil {
			continue
		}

		udp := udpLayer.(*layers.UDP)
		packets = append(packets, udpPacket{
			timestamp: ci.Timestamp,
			payload:   append([]byte(nil), udp.Payload...),
		})
	}

	return packets, nil
}

func replayPackets(conn net.Conn, packets []udpPacket) error {
	var previousTimestamp time.Time

	for i, packet := range packets {
		if i > 0 {
			delay := packet.timestamp.Sub(previousTimestamp)
			if delay > 0 {
				time.Sleep(delay)
			}
		}

		if _, err := conn.Write(packet.payload); err != nil {
			return fmt.Errorf("send packet %d: %w", i+1, err)
		}

		previousTimestamp = packet.timestamp
	}

	return nil
}
