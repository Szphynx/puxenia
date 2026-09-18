# Changelog

## 0.3.0
- Master volume: Push3's dedicated hardware Volume encoder now drives `channel_volume` directly, on any page/bank.
- Pitch bend: was silently dropped entirely (no handler existed); now forwarded to the device.
- Shift + touch strip now drives `mod_wheel` instead of pitch bend.
- New MIDI receive-channel filter (SETTINGS page's 4th column + web UI) restricts which channel's notes from external sources (Live clips, other gear) trigger a voice, so multiple routed tracks don't all sound at once. Push's own pad grid is never filtered this way.
- Persistent master-out level meter now shown on every page (was previously dead code — gated on a `"volume"` key that never matched Xenia's real `channel_volume` key, so it never actually appeared anywhere).
- PRESETS page: fixed "no presets found" — it was reading a Braids-specific `.braids` file format that doesn't exist for Xenia at all. Now shows all 128 Program-Change slots directly, and the Load button actually works (previously wrote to a `"preset"` key the device never recognized).
- On-screen color scheme inverted to a dark-navy/amber backlit-LCD look (previous mapping washed out on Push's screen); Volume fader widened for legibility.
- Removed stale dead code: unused CC74/71 constants left over from before the real Microwave XT CC chart was transcribed.
- Version now shown bottom-right in the web UI.

## 0.2.0
- Full 82-parameter, 2-bank on-screen UI across 11 real Microwave XT sections, Microwave-XT-inspired Push-native theme.
- Live CPU%/active-voice diagnostics in the web UI.
- Program-change debounce to fix voice dropout when cycling patches quickly.
- push-manager + push-display integration for on-screen encoder controls.
