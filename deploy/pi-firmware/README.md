# Pi firmware settings

admin's **Pi settings** section changes the Raspberry Pi 5's CPU clock
(`arm_freq`), extra voltage (`over_voltage_delta`), temperature limit
(`temp_limit`) and fan curve (`dtparam=fan_temp*`), and the CPU governor.
The ranges are in `web/internal/pifirmware/settings.go`; anything else is
refused. `force_turbo` is left out on purpose: with any extra voltage it
sets the SoC's permanent warranty bit, and the `performance` governor
gives the same speed without it.

## How a change is tried

1. **Save** writes the settings to `/boot/firmware/tryboot.txt`, a copy of
   `config.txt` with admin's block (`# BEGIN admin` … `# END admin`) at the
   end. `config.txt` isn't touched.
2. **Reboot to try them** (the server must be stopped, and you type
   `reboot`) runs `systemctl reboot "0 tryboot"`. The firmware reads
   `tryboot.txt` instead of `config.txt` for that one boot only.
3. `pi-firmware-boot.service` checks the trial boot: admin answers, no
   under-voltage, and a minute of full CPU load gets the right answers.
   If a check fails it reboots, and the Pi comes back on `config.txt`.
   If the Pi hangs or crashes, the hardware watchdog resets it, with the
   same result. Any reset or power cut also goes back to `config.txt`.
4. **Keep** backs `config.txt` up to `/boot/firmware/admin-backup/` (the
   newest 5 are kept) and writes the new block into it. **Go back** drops
   them; reboot to switch back. A reboot before Keep also goes back.

The admin block is rebuilt from numbers every time, and a block that was
edited by hand is refused rather than overwritten.

## Set up on the Pi (once)

Nothing here is done by a deploy: the helper runs as root, so it's
installed by hand and never updated by mc-agent. Run these on the Pi from
the active release.

Install the helper, root-owned:

```bash
sudo install -o root -g root -m 0755 ~/apps/current/bin/pi-firmware /usr/local/sbin/pi-firmware
```

Let admin's user run it (and only its subcommands) through sudo. `visudo`
checks the file before it's installed:

```bash
sed "s/PIUSER/$USER/" ~/apps/current/deploy/pi-firmware/sudoers > /tmp/pi-firmware.sudoers && sudo visudo -cf /tmp/pi-firmware.sudoers && sudo install -o root -g root -m 0440 /tmp/pi-firmware.sudoers /etc/sudoers.d/pi-firmware && rm /tmp/pi-firmware.sudoers
```

Install the boot check:

```bash
sudo install -o root -g root -m 0644 ~/apps/current/deploy/pi-firmware/pi-firmware-boot.service /etc/systemd/system/ && sudo systemctl daemon-reload && sudo systemctl enable pi-firmware-boot.service
```

Turn on systemd's hardware watchdog (Raspberry Pi OS leaves it off), then
check it says `active`:

```bash
sudo install -D -o root -g root -m 0644 ~/apps/current/deploy/pi-firmware/watchdog.conf /etc/systemd/system.conf.d/watchdog.conf && sudo systemctl daemon-reexec
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

If the account also has blanket passwordless sudo (Raspberry Pi OS sets
this up for the first user in `/etc/sudoers.d/010_pi-nopasswd`), anything
running as that user can already become root, so the narrow rule above
only limits admin if that file is removed.

## Updating the helper

A release that changes `web/cmd/pi-firmware` or `web/internal/pifirmware`
needs the install step again. `pi-firmware version` prints the release it
came from.

## Recovering by hand

`config.txt` is only ever written by Keep, after a backup in
`/boot/firmware/admin-backup/`. The boot partition is FAT, so any computer
can read it from the SD card: delete the admin block from `config.txt`, or
copy a backup over it, to go back to the firmware's defaults.
