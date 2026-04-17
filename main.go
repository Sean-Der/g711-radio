package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	frameSizeBytes = 160
	sampleRateHz   = 8000
	skipBytes      = 12
	configPath     = "config.local.json"
)

//go:embed web/*
var webFiles embed.FS

type appConfig struct {
	HTTPPort int            `json:"httpPort"`
	Streams  []streamConfig `json:"streams"`
}

type streamConfig struct {
	StreamName string `json:"streamName"`
	UDPPort    int    `json:"udpPort"`
}

type streamInfo struct {
	ID         string `json:"id"`
	StreamName string `json:"streamName"`
	UDPPort    int    `json:"udpPort"`
}

type station struct {
	info          streamInfo
	codec         webrtc.RTPCodecCapability
	frameDuration time.Duration
	logger        *log.Logger

	nextID      atomic.Uint64
	mu          sync.RWMutex
	subscribers map[string]*subscriber
}

type subscriber struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
}

type offerRequest struct {
	StreamID string                    `json:"streamId"`
	Offer    webrtc.SessionDescription `json:"offer"`
}

type webrtcServer struct {
	api        *webrtc.API
	logger     *log.Logger
	streams    map[string]*station
	streamList []streamInfo
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags)

	config, err := loadConfig(configPath)
	if err != nil {
		logger.Fatal(err)
	}

	codec := webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypePCMU,
		ClockRate: sampleRateHz,
		Channels:  1,
	}

	frameDuration := time.Second * frameSizeBytes / sampleRateHz

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		logger.Fatal(err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	server := &webrtcServer{
		api:        api,
		logger:     logger,
		streams:    make(map[string]*station, len(config.Streams)),
		streamList: make([]streamInfo, 0, len(config.Streams)),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, cfg := range config.Streams {
		info := streamInfo{
			ID:         fmt.Sprintf("stream-%d", cfg.UDPPort),
			StreamName: cfg.StreamName,
			UDPPort:    cfg.UDPPort,
		}

		st := &station{
			info:          info,
			codec:         codec,
			frameDuration: frameDuration,
			logger:        logger,
			subscribers:   make(map[string]*subscriber),
		}

		conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", cfg.UDPPort))
		if err != nil {
			logger.Fatalf("listen on UDP %d for %q: %v", cfg.UDPPort, cfg.StreamName, err)
		}

		server.streams[info.ID] = st
		server.streamList = append(server.streamList, info)

		logger.Printf(
			"configured stream %q on UDP %d, codec=PCMU, frame_size=%d bytes, skip_bytes=%d, frame_duration=%s",
			info.StreamName,
			info.UDPPort,
			frameSizeBytes,
			skipBytes,
			frameDuration,
		)

		go func(st *station, conn net.PacketConn) {
			<-ctx.Done()
			_ = conn.Close()
			st.closeSubscribers()
		}(st, conn)

		go func(st *station, conn net.PacketConn) {
			if err := st.ingest(ctx, conn); err != nil {
				logger.Printf("%s ingest stopped: %v", st.info.StreamName, err)
				stop()
			}
		}(st, conn)
	}

	staticFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		logger.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/streams", server.handleStreams)
	mux.HandleFunc("/offer", server.handleOffer)

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", config.HTTPPort),
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Printf("loaded %d stream(s) from %s", len(server.streamList), configPath)
	logger.Printf("serving WebRTC client on http://localhost:%d", config.HTTPPort)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
}

func loadConfig(path string) (appConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return appConfig{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	var config appConfig
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return appConfig{}, fmt.Errorf("decode %s: %w", path, err)
	}

	if config.HTTPPort < 1 || config.HTTPPort > 65535 {
		return appConfig{}, fmt.Errorf("%s has invalid httpPort %d", path, config.HTTPPort)
	}
	if len(config.Streams) == 0 {
		return appConfig{}, fmt.Errorf("%s has no streams configured", path)
	}

	seenPorts := make(map[int]struct{}, len(config.Streams))
	for i, stream := range config.Streams {
		if stream.StreamName == "" {
			return appConfig{}, fmt.Errorf("%s entry %d is missing streamName", path, i)
		}
		if stream.UDPPort < 1 || stream.UDPPort > 65535 {
			return appConfig{}, fmt.Errorf("%s entry %d has invalid udpPort %d", path, i, stream.UDPPort)
		}
		if _, exists := seenPorts[stream.UDPPort]; exists {
			return appConfig{}, fmt.Errorf("%s has duplicate udpPort %d", path, stream.UDPPort)
		}
		seenPorts[stream.UDPPort] = struct{}{}
	}

	return config, nil
}

func (s *station) ingest(ctx context.Context, conn net.PacketConn) error {
	buffer := make([]byte, 64*1024)
	packetsSeen := 0

	for {
		n, remoteAddr, err := conn.ReadFrom(buffer)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		frame, err := extractAudioFrame(buffer[:n])
		if err != nil {
			s.logger.Printf("%s: dropping UDP packet from %s: %v", s.info.StreamName, remoteAddr, err)
			continue
		}

		if packetsSeen == 0 {
			s.logger.Printf(
				"%s: first UDP packet from %s, packet_bytes=%d, audio_bytes=%d, skip_bytes=%d",
				s.info.StreamName,
				remoteAddr,
				n,
				frameSizeBytes,
				skipBytes,
			)
		}
		packetsSeen++

		s.broadcast(media.Sample{
			Data:     frame,
			Duration: s.frameDuration,
		})
	}
}

func (s *station) addSubscriber(pc *webrtc.PeerConnection) (string, error) {
	track, err := webrtc.NewTrackLocalStaticSample(s.codec, "audio", s.info.ID)
	if err != nil {
		return "", err
	}

	sender, err := pc.AddTrack(track)
	if err != nil {
		return "", err
	}

	go drainRTCP(sender)

	id := fmt.Sprintf("%s-peer-%d", s.info.ID, s.nextID.Add(1))

	s.mu.Lock()
	s.subscribers[id] = &subscriber{
		pc:    pc,
		track: track,
	}
	s.mu.Unlock()

	return id, nil
}

func (s *station) removeSubscriber(id string) {
	s.mu.Lock()
	sub, ok := s.subscribers[id]
	if ok {
		delete(s.subscribers, id)
	}
	s.mu.Unlock()

	if ok && sub.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
		_ = sub.pc.Close()
	}
}

func (s *station) closeSubscribers() {
	s.mu.Lock()
	subscribers := s.subscribers
	s.subscribers = make(map[string]*subscriber)
	s.mu.Unlock()

	for _, sub := range subscribers {
		if sub.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
			_ = sub.pc.Close()
		}
	}
}

func (s *station) broadcast(sample media.Sample) {
	s.mu.RLock()
	targets := make(map[string]*webrtc.TrackLocalStaticSample, len(s.subscribers))
	for id, sub := range s.subscribers {
		targets[id] = sub.track
	}
	s.mu.RUnlock()

	for id, track := range targets {
		if err := track.WriteSample(sample); err != nil {
			s.logger.Printf("%s: dropping %s after track write failure: %v", s.info.StreamName, id, err)
			s.removeSubscriber(id)
		}
	}
}

func (s *webrtcServer) handleStreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.streamList); err != nil {
		http.Error(w, "failed to encode streams", http.StatusInternalServerError)
	}
}

func (s *webrtcServer) handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()

	var request offerRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid offer body", http.StatusBadRequest)
		return
	}

	if request.StreamID == "" {
		http.Error(w, "missing streamId", http.StatusBadRequest)
		return
	}
	if request.Offer.Type != webrtc.SDPTypeOffer {
		http.Error(w, "expected an SDP offer", http.StatusBadRequest)
		return
	}

	station, ok := s.streams[request.StreamID]
	if !ok {
		http.Error(w, "unknown stream", http.StatusNotFound)
		return
	}

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, "failed to create peer connection", http.StatusInternalServerError)
		return
	}

	peerID, err := station.addSubscriber(pc)
	if err != nil {
		_ = pc.Close()
		http.Error(w, "failed to add audio track", http.StatusInternalServerError)
		return
	}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.logger.Printf("%s %s state: %s", station.info.StreamName, peerID, state.String())
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			station.removeSubscriber(peerID)
		}
	})

	if err := pc.SetRemoteDescription(request.Offer); err != nil {
		station.removeSubscriber(peerID)
		http.Error(w, "failed to apply remote description", http.StatusBadRequest)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		station.removeSubscriber(peerID)
		http.Error(w, "failed to create answer", http.StatusInternalServerError)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		station.removeSubscriber(peerID)
		http.Error(w, "failed to set local description", http.StatusInternalServerError)
		return
	}

	<-gatherComplete

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(pc.LocalDescription()); err != nil {
		station.removeSubscriber(peerID)
	}
}

func extractAudioFrame(payload []byte) ([]byte, error) {
	if len(payload) < skipBytes+frameSizeBytes {
		return nil, fmt.Errorf("packet is %d bytes, need at least %d", len(payload), skipBytes+frameSizeBytes)
	}

	return bytes.Clone(payload[skipBytes : skipBytes+frameSizeBytes]), nil
}

func drainRTCP(sender *webrtc.RTPSender) {
	buffer := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buffer); err != nil {
			return
		}
	}
}
