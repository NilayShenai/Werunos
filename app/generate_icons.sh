#!/bin/bash
set -e

# Make sure we have the input file
SRC_IMG="app/AppIcon.png"
if [ ! -f "$SRC_IMG" ]; then
    echo "Error: Source image $SRC_IMG not found!"
    exit 1
fi

ICONSET_DIR="app/AppIcon.iconset"
rm -rf "$ICONSET_DIR"
mkdir -p "$ICONSET_DIR"

# Generate the various PNG files using sips
sips -s format png -z 16 16     "$SRC_IMG" --out "$ICONSET_DIR/icon_16x16.png" > /dev/null 2>&1
sips -s format png -z 32 32     "$SRC_IMG" --out "$ICONSET_DIR/icon_16x16@2x.png" > /dev/null 2>&1
sips -s format png -z 32 32     "$SRC_IMG" --out "$ICONSET_DIR/icon_32x32.png" > /dev/null 2>&1
sips -s format png -z 64 64     "$SRC_IMG" --out "$ICONSET_DIR/icon_32x32@2x.png" > /dev/null 2>&1
sips -s format png -z 128 128   "$SRC_IMG" --out "$ICONSET_DIR/icon_128x128.png" > /dev/null 2>&1
sips -s format png -z 256 256   "$SRC_IMG" --out "$ICONSET_DIR/icon_128x128@2x.png" > /dev/null 2>&1
sips -s format png -z 256 256   "$SRC_IMG" --out "$ICONSET_DIR/icon_256x256.png" > /dev/null 2>&1
sips -s format png -z 512 512   "$SRC_IMG" --out "$ICONSET_DIR/icon_256x256@2x.png" > /dev/null 2>&1
sips -s format png -z 512 512   "$SRC_IMG" --out "$ICONSET_DIR/icon_512x512.png" > /dev/null 2>&1
sips -s format png -z 1024 1024 "$SRC_IMG" --out "$ICONSET_DIR/icon_512x512@2x.png" > /dev/null 2>&1

# Convert iconset to icns
iconutil -c icns "$ICONSET_DIR"

# Cleanup
rm -rf "$ICONSET_DIR"
echo "Successfully generated app/AppIcon.icns"
