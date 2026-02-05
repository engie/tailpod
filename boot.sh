#!/usr/bin/env bash

# Start with a blank disk
cp -c fedora-coreos-43.20260202.1.1-applehv.aarch64.raw.orig fedora-coreos-43.20260202.1.1-applehv.aarch64.raw
rm -f efi-variable-store
vfkit \
  --cpus 2 \
  --memory 2048 \
  --bootloader efi,variable-store=efi-variable-store,create \
  --device virtio-blk,path=fedora-coreos-43.20260202.1.1-applehv.aarch64.raw \
  --device virtio-net,nat,mac=02:42:ac:11:00:01 \
  --device virtio-input,keyboard \
  --device virtio-input,pointing \
  --device virtio-gpu,width=800,height=600 \
  --gui \
  --ignition tailpod.ign
