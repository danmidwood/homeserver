#!/bin/sh
set -e

INPUT_ISO=$1
ISO_FILENAME=$(basename $INPUT_ISO)
OUTPUT_ISO="${ISO_FILENAME=%.iso}_install_script.iso"

if [[ "$ISO_FILENAME" == *.iso ]]; then
    xorriso -indev ${INPUT_ISO} \
            -outdev ${OUTPUT_ISO} \
            -map ./install.sh /install.sh -- \
            -boot_image any replay
else
    echo "Invalid input"
fi
