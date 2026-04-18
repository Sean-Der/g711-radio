# G.711 Radio

This is a small Go WebRTC server built with Pion. It reads a local JSON config file, listens on one UDP port per configured stream, extracts a 160-byte G.711 audio frame from each packet, and broadcasts each stream to browser clients over WebRTC.

## Run it

```bash
go mod tidy
go run .
```

Edit the gitignored `config.local.json`, send your UDP audio to the configured ports, then open `http://localhost:<httpPort>`.

## Config

`config.local.json` contains the HTTP port and the UDP stream groups:

```json
{
  "httpPort": 8080,
  "streams": {
    "Studios": [
      { "streamName": "Studio A", "udpPort": 2250 },
      { "streamName": "Studio B", "udpPort": 2251 }
    ],
    "Remote Feeds": [
      { "streamName": "Field 1", "udpPort": 2260 }
    ]
  }
}
```

## Notes

- The server assumes 8 kHz mono G.711 frames and uses `160` audio bytes per packet, which maps to 20 ms of audio.
- The server is hard-coded for `PCMU` and strips a `12` byte transport header before reading each `160` byte audio frame.
- The sample client loads grouped streams from `/streams`, renders them as collapsed accordions, and opens each feed in its own dedicated tab.
- The sample client uses non-trickle ICE and is intended for local or simple LAN testing. If you need internet-facing playback, add STUN/TURN configuration.

## Test with GStreamer

An example sender lives at [examples/gst-launch-pcmu-sine.sh](/Users/seaduboi/code/g711-radio/examples/gst-launch-pcmu-sine.sh:1). It generates an 8 kHz mono sine wave, encodes it as `PCMU`, packetizes it as RTP, and sends it to `127.0.0.1:2250`.

## Debugging with pcapng

```bash
go run ./cmd/replay-pcap -pcap g711.pcapng -addr 127.0.0.1:2250
```
