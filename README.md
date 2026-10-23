# music-server

Download the latest installer and run it. The same command installs a
new server or upgrades one that is already installed.

```
wget -O /tmp/install.sh https://github.com/sumitgahoi/music-server/releases/latest/download/install.sh
sudo bash /tmp/install.sh
```

On a new install it asks for the music folder. It then downloads the
binary for this machine (linux/amd64 or linux/arm64), installs the
systemd service, and starts it. The service starts on boot and
restarts after a crash.

Uninstall with the script installed next to the binary:

```
sudo /usr/local/bin/uninstall.sh
```

It asks whether to leave the database and artwork, or remove them.
The music folder stays.

```
sudo /usr/local/bin/uninstall.sh --keep-data
sudo /usr/local/bin/uninstall.sh --purge
```
