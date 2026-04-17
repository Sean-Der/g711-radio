# G.711 Radio

This is a small Go WebRTC server built with Pion. It reads a local JSON config file, listens on one UDP port per configured stream, extracts a 160-byte G.711 audio frame from each packet, and broadcasts each stream to browser clients over WebRTC.

## Run it

```bash
go mod tidy
go run .
```

Edit the gitignored `config.local.json`, send your UDP audio to the configured ports, then open `http://localhost:<httpPort>`.

## Config

`config.local.json` contains the HTTP port and the UDP stream list:

```json
{
  "httpPort": 8080,
  "streams": [
    { "streamName": "Studio A", "udpPort": 2250 },
    { "streamName": "Studio B", "udpPort": 2251 }
  ]
}
```

## Notes

- The server assumes 8 kHz mono G.711 frames and uses `160` audio bytes per packet, which maps to 20 ms of audio.
- The server is hard-coded for `PCMU` and strips a `12` byte transport header before reading each `160` byte audio frame.
- The sample client loads all configured streams from `/streams` and lets you connect to one stream at a time.
- The sample client uses non-trickle ICE and is intended for local or simple LAN testing. If you need internet-facing playback, add STUN/TURN configuration.
