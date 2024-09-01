#!/bin/bash
set -e

# Set up variables
DISK="/dev/nvme0n1"

# Partition the disk
sfdisk ${DISK} <<EOF
label: gpt
label-id: 41F3BE8C-1112-4855-A3FA-DBDB964044B2
device: /dev/nvme0n1
unit: sectors
first-lba: 34
last-lba: 1000215182
sector-size: 512

/dev/nvme0n1p1 : start=        2048, size=      999424, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, uuid=CA4BE9DE-3CE8-45E7-8EDB-6B6479D84EC6
/dev/nvme0n1p2 : start=     1001472, size=    58593280, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, uuid=B6CF6ED9-2D3B-4B95-857F-40FD1A9F4882
/dev/nvme0n1p3 : start=    59594752, size=    97656832, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, uuid=512E0B6E-8CAC-442D-AD0E-ABD3B0396DFC
/dev/nvme0n1p4 : start=   157251584, size=    97656832, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, uuid=A170A286-E4E6-4F65-8B27-12D6338F7B8E
/dev/nvme0n1p5 : start=   254908416, size=   745306112, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, uuid=841ABEAA-3AE5-4880-A91C-5A103138B5C8

EOF

# Format partitions
mkfs.btrfs -f ${DISK}p2 # root
mkfs.fat -F32 ${DISK}p1 # Format the EFI partition as FAT32
mkfs.btrfs -f ${DISK}p3 # home
mkfs.btrfs -f ${DISK}p4 # var
mkfs.btrfs -f ${DISK}p5 # storage

# Mount partitions
mkdir -p /mnt;             mount ${DISK}p2 /mnt
mkdir /mnt/boot;           mount ${DISK}p1 /mnt/boot
mkdir /mnt/home;           mount ${DISK}p3 /mnt/home
mkdir /mnt/var;            mount ${DISK}p4 /mnt/var
mkdir -p /mnt/mnt/storage; mount ${DISK}p5 /mnt/mnt/storage
chmod 777 /mnt/mnt/storage # Make this one accessible to all

# Install base system
pacstrap /mnt base linux linux-firmware

# Generate fstab
genfstab -U /mnt >> /mnt/etc/fstab

# Chroot into system
arch-chroot /mnt /bin/bash <<EOF
# Install bootloader
pacman -S --noconfirm grub efibootmgr
grub-install --target=x86_64-efi --efi-directory=/boot --bootloader-id=GRUB
grub-mkconfig -o /boot/grub/grub.cfg

# Install additional packages
pacman -S --noconfirm git ansible

# Download install repo
git clone https://github.com/danmidwood/homeserver.git /root/homeserver

cd /root/homeserver

ansible-playbook -i inventory/hosts_xps_local.ini playbooks/xps_chroot.yml

EOF

# Unmount partitions and reboot
umount -R /mnt
reboot
