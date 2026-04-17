#!/bin/sh

# Sends a live RTP/PCMU sine wave to localhost:2250.
# This matches the server's current assumptions:
# - 8 kHz mono
# - PCMU
# - 12-byte RTP header
# - 160-byte audio payload per packet

exec gst-launch-1.0 -v \
  audiotestsrc is-live=true wave=sine freq=440 samplesperbuffer=160 ! \
  audio/x-raw,format=S16LE,rate=8000,channels=1 ! \
  mulawenc ! \
  audio/x-mulaw,rate=8000,channels=1 ! \
  rtppcmupay pt=0 mtu=172 min-ptime=20000000 max-ptime=20000000 ! \
  udpsink host=127.0.0.1 port=2250 sync=true async=false
