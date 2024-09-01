# Homeserver IaC

This is my config to set up a Dell XPS 9360 as a home server.

## Steps

* Download an Arch linux ISO
* Run `./mkiso.sh $path_to_arch_iso`
* Write the iso to a usb pen drive with `sudo dd if=./archlinux-xxx.install_script.iso_ of=/path/to/usb/device bs=4M status=progress`
* Boot XPS from this usb
* Connect to internet, `iwctl station wlan0 connect "wireless network name"`
* Copy the install script from usb
  * `mkdir archiso`
  * `mount /dev/sda1 archiso`
  * `cp archiso/install.sh .`
  * `umount /dev/sda1`
* Run the install script `./install.sh`
  * This will install Arch, run ansible to install things, then restart the system

Updating the iso and extracting the script can be dropped in favour of pulling the script directly from github after connecting to the internet, this command will fetch and run it `sh -c "$(curl -fsSL https://raw.githubusercontent.com/danmidwood/homeserver/master/install.sh)"`
