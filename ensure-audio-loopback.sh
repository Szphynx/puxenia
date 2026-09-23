# ensure-audio-loopback.sh -- (re)loads the snd-aloop kernel module on
# Push if the "Audio" loopback card isn't already up. /tmp is tmpfs
# (wiped on every Push reboot) and push-hack-audio-loopback was never
# installed persistently via the catalog, so this needs to be redone
# after every reboot -- see docs/environment-and-deploy.md.
#
# Sourced (not executed directly) by deploy.sh and deploy-all.sh -- both
# must already have PUSH_HOST/PUSH_KEY set before calling
# ensure_audio_loopback. Split out of push-hack-xenia's own deploy.sh
# (where this logic used to live exclusively) because it's a system-level
# prerequisite shared by every hack that plays audio (push-hack-xenia AND
# push-hack-mm), not something specific to Xenia. A deploy-all.sh run
# that skips Xenia (e.g. `--mm --hub`, to avoid rebuilding its slow C++
# plugin) used to silently skip this reload too -- after a Push reboot,
# that left the loopback card missing entirely, with "the loopback
# driver doesn't appear on my push" and no obvious reason why, since
# nothing about --mm --hub looked audio-related at all.

ensure_audio_loopback() {
    local aloop_ko_dir="${ALOOP_KO_DIR:-$HOME/push-hack-audio-loopback/ko}"
    local ssh_key="$PUSH_KEY"
    local push_host="$PUSH_HOST"

    _ealb_ssh() { ssh -i "$ssh_key" "root@${push_host}" "$@"; }
    _ealb_scp() { scp -i "$ssh_key" "$@"; }

    # push-xenia/push-mm look for card id "Audio" -- NOT the "PHVAudio"
    # driver name shown in /proc/asound/cards's second column. That id is
    # ALSA's auto-derived id for this module's longname ("Push Hack
    # Virtual Audio"), since it's insmod'd with no id= override. Don't
    # try to force id=PHVAudio here: once Live has opened the card's PCM
    # devices, an id= reload requires rmmod first, which fails EBUSY
    # while Live holds it open -- forcing that would mean killing Live
    # just to rename an already-working card.
    if _ealb_ssh "grep -qE '^\s*[0-9]+ \[Audio *\]' /proc/asound/cards 2>/dev/null"; then
        return 0
    fi

    local kver ko_local
    kver="$(_ealb_ssh "uname -r")"
    ko_local="$aloop_ko_dir/$kver/snd-aloop.ko"
    if [[ -f "$ko_local" ]]; then
        echo "   Loading bundled snd-aloop.ko for kernel $kver (push-hack-audio-loopback"
        echo "   was never installed persistently via the catalog, so this is redone"
        echo "   on every reboot -- see push-hack-audio-loopback/README.md to install"
        echo "   it properly instead)."
        _ealb_ssh "mkdir -p /tmp/audio-loopback-setup"
        _ealb_scp "$ko_local" "root@${push_host}:/tmp/audio-loopback-setup/snd-aloop.ko"
        _ealb_ssh "insmod /tmp/audio-loopback-setup/snd-aloop.ko"
    else
        echo "   WARNING: no bundled snd-aloop.ko for kernel $kver in $aloop_ko_dir --"
        echo "   audio loopback will not work. See push-hack-audio-loopback/README.md"
        echo "   to build one for this kernel."
    fi
}
