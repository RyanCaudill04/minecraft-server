# Pi firmware settings

admin's **Pi settings** section changes the Raspberry Pi 5's CPU clock
(`arm_freq`), extra voltage (`over_voltage_delta`), temperature limit
(`temp_limit`) and fan curve (`dtparam=fan_temp*`), and the CPU governor.
The ranges are in `web/internal/pifirmware/settings.go`; anything else is
refused. `force_turbo` is never turned on: with any extra voltage it sets
the SoC's permanent warranty bit, so admin's block always ends with
`force_turbo=0`, which wins over a `force_turbo=1` anywhere above it. The
`performance` governor gives the same speed without it.

## How a change is tried

1. **Save** writes the settings to `/boot/firmware/tryboot.txt`, a copy of
   `config.txt` with admin's block (`# BEGIN admin` … `# END admin`) at the
   end. `config.txt` isn't touched.
2. **Reboot to try them** (the server must be stopped, and you type
   `reboot`) runs `systemctl reboot "0 tryboot"`. The firmware reads
   `tryboot.txt` instead of `config.txt` for that one boot only.
3. On that boot, `pi-firmware-boot.service`:
   - keeps `minecraft.service` stopped, so Paper never runs on settings
     that haven't been kept (admin refuses to start or wake it too);
   - checks the Pi: three minutes of every core hashing 256 MB, under the
     `performance` governor, polling `vcgencmd get_throttled` and the
     clock every second. It fails on a wrong hash, under-voltage, a capped
     clock or throttling, or a clock that never reaches the `arm_freq`
     being tried;
   - reboots back to `config.txt` if the check fails, or if the settings
     aren't kept within 30 minutes.

   A hang or crash resets the Pi through the hardware watchdog, and any
   reset or power cut also comes back on `config.txt`.
4. **Keep** backs `config.txt` up to `/boot/firmware/admin-backup/` (the
   newest 5 are kept) and writes the new block into it. **Go back** drops
   them; reboot to switch back.

### Probation after Keep

For 14 days after Keep, every boot checks whether the one before it ended
cleanly (the boot unit marks a clean shutdown). After **two unclean
resets** in that time (a crash, a watchdog reset or a power cut) it puts
back the `config.txt` from before Keep and reboots onto it, and admin says
so. Power cuts count too, so two in a fortnight roll back settings that
were fine; try them again.

Every admin block, kept ones included, sets `kernel_watchdog_timeout`, so
settings that hang the Pi, even early in boot, reset it rather than leave
it stuck. That needs systemd serving the watchdog (`watchdog.conf` below):
**don't remove `watchdog.conf` while `config.txt` has an admin block**, or
the Pi resets about a minute after every boot.

What probation can't fix: settings that hang the Pi every time before
systemd starts would reset it over and over without the boot unit ever
running to count. Only settings that passed a trial boot get kept, which
makes that unlikely, but the way out then is by hand (below).

The admin block is rebuilt from numbers every time, and a block that was
edited by hand is refused rather than overwritten.

## Set up on the Pi (once)

Nothing here is done by a deploy: the helper runs as root, so it's
installed by hand and never updated by mc-agent. Install it from a copy
your user can't change: download a release as root into a folder only
root can write, and check it against its checksum. `~/apps/current` is
writable by that user, so don't install from there.

Pick the release (`rN`, as admin's Deploys section shows it), and
download it as root:

```bash
tag=rN
```

```bash
sudo rm -rf /root/pi-firmware-src && sudo install -d -m 0700 /root/pi-firmware-src && sudo curl -fsSL -o /root/pi-firmware-src/release.tar.gz "https://github.com/RyanCaudill04/minecraft-server/releases/download/$tag/release.tar.gz" && sudo curl -fsSL -o /root/pi-firmware-src/release.tar.gz.sha256 "https://github.com/RyanCaudill04/minecraft-server/releases/download/$tag/release.tar.gz.sha256"
```

```bash
sudo sh -c 'cd /root/pi-firmware-src && sha256sum -c release.tar.gz.sha256 && tar -xzf release.tar.gz ./bin/pi-firmware ./deploy/pi-firmware'
```

Read each file before it's installed:

```bash
sudo cat /root/pi-firmware-src/deploy/pi-firmware/sudoers /root/pi-firmware-src/deploy/pi-firmware/pi-firmware-boot.service /root/pi-firmware-src/deploy/pi-firmware/watchdog.conf
```

Install the helper, root-owned:

```bash
sudo install -o root -g root -m 0755 /root/pi-firmware-src/bin/pi-firmware /usr/local/sbin/pi-firmware
```

Let your user run it (and only its subcommands) through sudo. The rule is
written straight into a root-owned file that sudo ignores while its name
has a dot in it, and `visudo` checks that file before it's renamed into
use:

```bash
sudo sh -c "sed 's/PIUSER/$USER/' /root/pi-firmware-src/deploy/pi-firmware/sudoers > /etc/sudoers.d/pi-firmware.new" && sudo chmod 0440 /etc/sudoers.d/pi-firmware.new && sudo visudo -cf /etc/sudoers.d/pi-firmware.new && sudo mv /etc/sudoers.d/pi-firmware.new /etc/sudoers.d/pi-firmware
```

Install the boot unit, with your user as the one whose
`minecraft.service` a trial boot keeps stopped:

```bash
sudo sh -c "sed 's/PIUSER/$USER/' /root/pi-firmware-src/deploy/pi-firmware/pi-firmware-boot.service > /etc/systemd/system/pi-firmware-boot.service" && sudo systemctl daemon-reload && sudo systemctl enable --now pi-firmware-boot.service
```

Turn on systemd's hardware watchdog (Raspberry Pi OS leaves it off), then
check it says `active`:

```bash
sudo install -D -o root -g root -m 0644 /root/pi-firmware-src/deploy/pi-firmware/watchdog.conf /etc/systemd/system.conf.d/watchdog.conf && sudo systemctl daemon-reexec
```

```bash
cat /sys/class/watchdog/watchdog0/state
```

Name the email that may change the settings in
`~/.config/minecraft/secrets.env` (see `deploy/secrets.example.env`), then
restart admin:

```bash
systemctl --user restart admin
```

## What the sudo rule allows

The rule is for the Unix user, not for admin, and play, Paper and its
plugins, mc-agent and the backup all run as that user. So anything
running as it can use these subcommands without admin's owner check or
typed confirmation:

- reboot the Pi while the Minecraft server is stopped;
- try any settings within the ranges, with a trial reboot;
- keep them once they pass the trial's hardware check. That check runs
  as root, and nothing running as the user can answer it; admin's own
  health isn't part of it.

So the worst such a process can do is keep the fastest allowed overclock
(3000 MHz, +50 mV). Probation puts the old `config.txt` back if that
resets the Pi twice in 14 days. The helper still checks every value, and
never writes anything but the admin block.

Running admin as a Unix user of its own, so only admin can use the rule,
would close this. It's left for a later change.

If the account also has blanket passwordless sudo (Raspberry Pi OS sets
this up for the first user in `/etc/sudoers.d/010_pi-nopasswd`), anything
running as that user can already become root, so the narrow rule above
only limits anything if that file is removed.

## Updating the helper

A release that changes `web/cmd/pi-firmware`, `web/internal/pifirmware`
or `deploy/pi-firmware` needs the steps above again, from that release.
`pi-firmware version` prints the release it came from.

## Recovering by hand

`config.txt` is only ever written by Keep, after a backup in
`/boot/firmware/admin-backup/`, and by probation putting a backup back.
The boot partition is FAT, so any computer can read it from the SD card:
delete the admin block from `config.txt`, or copy a backup over it, to go
back to the firmware's defaults.
