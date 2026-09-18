# Environment & deploy notes

Operational knowledge gained from actually building/deploying/debugging this
project on real hardware — not duplicated from code comments elsewhere in
this repo. Read this before repeating any of the discovery work below.

## The dev machine (WSL, Ubuntu 22.04) is unusually restricted

This is a locked-down WSL instance, not a normal Linux box. Several things
that "should just work" don't:

- **Docker is effectively unusable for running containers.** `docker.io`
  installs fine via apt, but:
  - There's no working init script — `dockerd` must be started manually:
    `sudo dockerd --iptables=false --bridge=none > ~/dockerd.log 2>&1 &`
    (`--iptables=false --bridge=none` are required — this WSL's netfilter
    only has `nft` support and `iptables-legacy`/`ip6tables` fail with
    `Failed to initialize nft: Protocol not supported`, and creating the
    `docker0` bridge fails with plain `permission denied` regardless).
  - Even with the daemon up, **actually running a container fails**:
    `docker run` returns `Unimplemented: unknown service
    containerd.services.tasks.v1.Tasks`. Root cause visible in
    `~/dockerd.log`: several containerd plugins (`mount-manager`,
    `runtime.v2.task`, `tasks-service`, etc.) fail to load with `failed to
    open database file: operation not permitted` — a bolt-db locking
    issue specific to this WSL setup. **Images pull fine, containers never
    run.** Don't burn time debugging this further; see the workaround
    below.
  - **Workaround used successfully**: skip Docker entirely for
    cross-compiling `push-display`'s `push_hook.so`. The Makefile's
    Docker step (`gcc:12-bullseye`) exists only to pin an older glibc for
    compatibility, not because of a CPU architecture difference — WSL and
    Push 3 are both `x86_64`. Compile natively instead:
    ```
    gcc -shared -fPIC -o push_hook.so src/push_hook.c -ldl -lpthread -O2 \
      -Wall -Wno-unused-function -D_GNU_SOURCE
    ```
    Then verify it'll actually load on Push:
    `objdump -T push_hook.so | grep -o "GLIBC_[0-9.]*" | sort -V | tail -5`
    — confirmed max `GLIBC_2.34` on the natively-built `.so`, and Push 3's
    own `/lib/libc.so.6` supports up to `GLIBC_2.35`, so it loads fine.

- **apt's Go is ancient (1.18.1) and this project's `go.mod` needs 1.25+.**
  - `sudo apt install golang-go` gets you 1.18 — `go build` fails with
    `invalid go version '1.25.0': must match format 1.23` (that error
    message is misleading; it's really "your Go is too old to parse this
    go.mod's version field," not a format complaint).
  - Fix: download the real toolchain and make sure it wins on `PATH` —
    apt's `/usr/bin/go` shadows `/usr/local/go/bin/go` otherwise even
    after installing the newer one:
    ```
    sudo apt remove -y golang-go
    curl -LO https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
    sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz
    sed -i '1i export PATH=/usr/local/go/bin:$PATH' ~/.bashrc && source ~/.bashrc
    go version   # must print go1.25.0, not go1.18.1
    ```

- **cgo builds need `libasound2-dev`** (not installed by default):
  `sudo apt install -y libasound2-dev` — needed for both
  `push-hack-xenia`'s Go host (links `-lasound`) and anything else that
  touches ALSA via cgo.

## Push 3 hardware quirks

- **No `/lib64` at all.** Push's rootfs only has `/lib`, but any binary
  built on a normal x86_64 Linux toolchain (WSL included) hardcodes
  `/lib64/ld-linux-x86-64.so.2` as its ELF interpreter. Binary runs fine
  by every check (`file`, `readelf`/`strings` for the interpreter path,
  correct arch, correct byte count after transfer) yet `./binary` fails
  with a bare `No such file or directory` — that error is the kernel
  failing to find the *interpreter*, not the binary itself. Fix, on Push
  (not WSL):
  ```
  ln -s /lib /lib64
  ```
  One-time per boot... actually check if `/tmp` being tmpfs wipes this
  too (see below) — if so, this needs redoing after every Push reboot,
  same as everything else deployed to `/tmp`.

- **`/tmp` is tmpfs — wiped on every reboot.** Anything deployed there
  (ROMs, `.so`, binaries, `hack.json`) needs a full redeploy after a
  Push reboot. The `snd-aloop` ALSA loopback kernel module also needs
  reloading as root after a reboot.

- **SSH: key auth only, password auth disabled server-side.** Don't waste
  time trying `-o PreferredAuthentications=password` — the server itself
  rejects it (`Permission denied (publickey)` even when password auth is
  forced client-side). Use whatever key was previously provisioned to
  Push's `authorized_keys` — in this project's case, `~/xenia-build/pushkey`
  (needs `chmod 600` if freshly copied — SSH refuses "unprotected private
  key" otherwise). `root@<ip>` works directly with that key; there's no
  need to go through the `ableton` user first.

- **Push's IP is dynamic** (no static DHCP reservation assumed) — find it
  from an existing shell on Push: `ip addr show | grep "inet "` → the
  `wlan0` entry, not `127.0.0.1`.

- **`scp`'ing over a running binary fails with `Text file busy`.** Kill
  the process first: `ssh root@<ip> "pkill -f push-xenia"`, then `scp`,
  then relaunch.

- **A binary built on WSL and one built on Push must match glibc within
  reason.** Confirmed compatible: WSL's build toolchain tops out at
  `GLIBC_2.34`/`GLIBCXX` symbols; Push 3's `/lib/libc.so.6` supports up to
  `2.35`. If a future dependency pushes past that, static-link the
  runtime that's the actual mismatch (see `xenia_render`'s CMakeLists:
  `-static-libstdc++ -static-libgcc` was needed there specifically
  because Push's `libstdc++.so.6` lacked `GLIBCXX_3.4.30` that the WSL
  build machine linked against, even though glibc itself matched exactly).

## Full deploy loop (once toolchain issues above are resolved)

```bash
cd ~/puxenia && git pull origin main

# C++ plugin
cd xenia-plugin/build && cmake --build . -j"$(nproc)"

# Go host
cd ~/puxenia/push-hack-xenia/src && go build -o push-xenia .

# Deploy (kill first if already running — see "Text file busy" above)
ssh -i ~/xenia-build/pushkey root@<PUSH_IP> "pkill -f push-xenia"
scp -i ~/xenia-build/pushkey ../../xenia-plugin/build/libxenia-plugin.so root@<PUSH_IP>:/tmp/xenia-hack/
scp -i ~/xenia-build/pushkey push-xenia root@<PUSH_IP>:/tmp/xenia-hack/
scp -i ~/xenia-build/pushkey -r ui root@<PUSH_IP>:/tmp/xenia-hack/

# Relaunch
ssh -i ~/xenia-build/pushkey root@<PUSH_IP>
cd /tmp/xenia-hack && ./push-xenia
```

## Git/GitHub setup gotchas

- GitHub password auth for `git push` is deprecated — need a Personal
  Access Token (repo scope) generated at
  `https://github.com/settings/tokens/new`, used as the password.
- `git commit` fails with "Author identity unknown" on a fresh machine —
  `git config --global user.email`/`user.name` once.
- SSH host-key changes after a Push OS update (`REMOTE HOST
  IDENTIFICATION HAS CHANGED`) — `ssh-keygen -R <host>` clears the stale
  entry (the `ableton-push-hack` framework's own installer scripts do
  this automatically; doing it by hand elsewhere needs the manual step).
