package main

// params.go — parameter metadata and live value/page state for the
// on-screen control UI (see display.go) and the 8 encoders + D-Pad
// Left/Right that drive it (see main.go's midiHandler).
//
// Metadata (name/type/min/max/enum options) comes straight from the DSP
// plugin itself via bridge_plugin_get_param("chain_params") — a JSON list
// the plugin already builds for its own generic-UI support (see
// braids_plugin.cpp's v2_get_param). Reading it here means this host never
// hardcodes Braids-specific ranges or the engine's shape names.

/*
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unsafe"
)

type paramMeta struct {
	Key     string   `json:"key"`
	Name    string   `json:"name"`
	Type    string   `json:"type"` // "float", "int", "enum"
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
	Options []string `json:"options"`
}

// paramPages curates which params sit on which page and in which encoder
// slot (index 0-7, left to right, matching CC 71-78) — see paramPages'
// doc below for what it points at and why it's a var, not a const.
//
// Push has exactly 8 top-screen buttons (push3.CCScreenTopN(0-7)), but
// the Microwave XT's real MIDI-controllable surface (verified against
// the actual "MIDI Controller Assignments" chart, software release
// 2.28 — every CC number in xenia_plugin.cpp's kParams table is
// transcribed from it, not guessed) is 82 params across 11 natural
// groups: OSC, MIXER, FILTER, FILTER ENV, AMP, LFO 1, LFO 2, GLOBAL,
// FREE ENV, ARP, WAVE MOD — plus PRESETS and SETTINGS, which were
// already there. That's 13 pages for 8 buttons.
//
// Fix: two banks of 8, flipped with D-Pad Left/Right (push3.CCDPadLeft/
// Right) — confirmed unused by this fork ("D-Pad/Select are unused now"
// per this file's own params.go history and main.go's Fixed(), which
// had no case for those CCs at all before this). PRESETS and SETTINGS
// are pinned to slots 6/7 in BOTH banks (bankPageNames/bankParamPages
// share the same tail), so they're reachable from either bank and the
// pagePresets/pageSettings constants stay valid regardless of which
// bank is active. Slots 0-5 hold each bank's own 6 sound-page groups
// (bank B's slot 5 is spare/nil — 11 groups split 6+5, not 6+6).
//
// paramPages/pageNames are package vars, not the bank tables themselves
// — setBank reassigns them to whichever bank's slice is now current, so
// every existing call site that already reads the package-level
// paramPages/pageNames (renderKnobGrid, renderTopTabs, leds.go's
// syncUILEDs, webParamPages) keeps working unchanged; only newParamState
// needed to change, to build every param's slot from BOTH banks up
// front (see allBankPages below), since a param's slot must exist
// whether or not its bank is the one currently on screen.
const (
	pagePresets  = 6
	pageSettings = 7
)

var bankPageNames = [2][8]string{
	{"OSC", "MIXER", "FILTER", "FILTER ENV", "AMP", "LFO 1", "PRESETS", "SETTINGS"},
	{"LFO 2", "GLOBAL", "FREE ENV", "ARP", "WAVE MOD", "", "PRESETS", "SETTINGS"},
}

var bankParamPages = [2][8][]string{
	{
		{"osc1_octave", "osc1_semitone", "osc1_detune", "osc1_wave", "osc2_octave", "osc2_semitone", "osc2_detune", "osc2_wave"},
		{"wave1_level", "wave2_level", "ringmod_level", "noise_level", "osc2_sync", "osc2_link", "fm_amount", "wavetable"},
		{"program", "cutoff", "resonance", "filter_type", "filter_keytrack", "filter_env_amount", "filter_env_velocity", "filter2_cutoff"},
		{"f_attack", "f_decay", "f_sustain", "f_release", "f_trigger"},
		{"a_attack", "a_decay", "a_sustain", "a_release", "a_velocity", "a_trigger", "amp_volume", "amp_keytrack"},
		{"lfo1_rate", "lfo1_shape", "lfo1_delay", "lfo1_sync", "lfo1_symmetry", "lfo1_humanize"},
		nil, // PRESETS
		nil, // SETTINGS
	},
	{
		{"lfo2_rate", "lfo2_shape", "lfo2_delay", "lfo2_sync", "lfo2_symmetry", "lfo2_humanize", "lfo2_phase"},
		{"mod_wheel", "channel_volume", "panning", "chorus", "sustain", "glide_time", "glide_type", "glide_mode"},
		{"fe_time1", "fe_level1", "fe_time2", "fe_level2", "fe_time3", "fe_level3", "fe_rel_time", "fe_rel_level"},
		{"arp_active", "arp_range", "arp_clock", "arp_tempo", "arp_direction", "arp_pattern", "arp_note_order", "arp_pattern_length"},
		{"wave1_phase", "wave1_env_amt", "wave1_env_vel", "wave1_keytrack", "wave2_phase", "wave2_env_amt", "wave2_env_vel", "wave2_keytrack"},
		nil, // spare
		nil, // PRESETS
		nil, // SETTINGS
	},
}

// currentBank tracks which of bankPageNames/bankParamPages paramPages and
// pageNames currently point at — see setBank.
var currentBank = 0

// paramPages/pageNames start on bank 0; setBank below reassigns both
// whenever the D-Pad flips banks. Every render/LED function already
// reads these two package vars directly, so reassigning them here is
// the entire bank-switch mechanism — nothing else needs to change.
var (
	paramPages = bankParamPages[0][:]
	pageNames  = bankPageNames[0][:]
)

// setBank flips to bank 0 or 1 (any other value is a no-op) — called
// from audiosession.go's drainCtl on a D-Pad Left/Right controlEvent —
// and jumps to that bank's first page, so the screen never shows a
// bank's top-tab labels while displaying the other bank's page.
func setBank(st *paramState, n int) {
	if n != 0 && n != 1 {
		return
	}
	currentBank = n
	paramPages = bankParamPages[n][:]
	pageNames = bankPageNames[n][:]
	st.setPage(0)
	st.MarkDirty()
}

// allBankPages flattens both banks' param keys for newParamState, which
// must build a slot for every param regardless of which bank is
// currently on screen — paramPages alone (whichever bank is active right
// now) would leave the other bank's params with no slot at all, so every
// encoder/web-UI write to them would silently no-op.
var allBankPages = append(append([][]string{}, bankParamPages[0][:]...), bankParamPages[1][:]...)

// paramSlot is one parameter's live state: its metadata plus the Go-side
// value driving the plugin. The plugin's get_param has no "current value"
// query for individual params (only the bulk "state" JSON), so this host's
// own value is the single source of truth for what an encoder set last.
type paramSlot struct {
	meta  paramMeta
	value float64
	// accum carries leftover encoder delta between messages for an enum
	// slot with reduced sensitivity (see enumSensitivity) — a turn that
	// hasn't yet crossed its threshold accumulates here instead of being
	// dropped, so slow deliberate turning still eventually lands a step.
	accum int
}

// enumSensitivity maps an enum param's key to how much accumulated encoder
// delta it takes to advance one option — 1 (the default, via
// sensitivityFor) steps on every message like every other enum; a value
// above that makes the knob "heavier". "program" is Xenia's 128-patch
// select, the same kind of "one graze shouldn't jump 20 patches" case
// Braids' "engine" (47 algorithms) originally motivated this for.
var enumSensitivity = map[string]int{
	"program":          4,
	"preset":           4,
	"octave_transpose": 4,
}

// sensitivityFor returns how much accumulated delta enum key needs before
// stepping once — see enumSensitivity.
func sensitivityFor(key string) int {
	if d, ok := enumSensitivity[key]; ok && d > 0 {
		return d
	}
	return 1
}

// paramState guards the current page and every param's value against
// concurrent access: the render loop goroutine (main.go) writes it on every
// encoder/page event, the display loop goroutine (display.go) reads it at
// ~30fps to redraw.
type paramState struct {
	mu    sync.Mutex
	page  int
	slots map[string]*paramSlot
	dirty bool

	// presetCursor/presetAccum: PRESETS page's staged (not-yet-loaded)
	// highlight — separate from slots["preset"].value, which only changes
	// once Load (bottom-1) is pressed. See movePresetCursor/loadStagedPreset.
	presetCursor int
	presetAccum  int
}

// fetchChainParams calls the plugin's get_param("chain_params") and parses
// the resulting JSON metadata list.
func fetchChainParams(plugin *C.bridge_plugin_t) ([]paramMeta, error) {
	const bufLen = 8192
	buf := make([]byte, bufLen)
	key := C.CString("chain_params")
	defer C.free(unsafe.Pointer(key))
	n := C.bridge_plugin_get_param(plugin, key, (*C.char)(unsafe.Pointer(&buf[0])), C.int(bufLen))
	if n < 0 {
		return nil, fmt.Errorf("get_param(chain_params) failed")
	}
	var metas []paramMeta
	if err := json.Unmarshal(buf[:n], &metas); err != nil {
		return nil, fmt.Errorf("parse chain_params JSON: %w", err)
	}
	for i := range metas {
		// "engine" (and any other enum) carries its range as "options",
		// not "min"/"max" — the plugin's chain_params JSON omits min/max
		// for enum entries entirely (see braids_plugin.cpp's chain_params
		// handler). Left at their zero value, every encoder turn clamped
		// straight back to 0 (CSAW) — the reported "engine stuck on
		// CSAW, won't scroll" bug.
		if metas[i].Type == "enum" && len(metas[i].Options) > 0 {
			metas[i].Min = 0
			metas[i].Max = float64(len(metas[i].Options) - 1)
		}
	}
	return metas, nil
}

// syncFromPluginState re-reads the plugin's own get_param("state") — the
// same JSON used for patch save/load — and overwrites every matching slot's
// value with it. Needed because paramSlot.value is normally the single
// source of truth (params.go's doc: the plugin has no per-param "current
// value" query), but a preset load bypasses that: v2_apply_preset
// overwrites the plugin's entire params[] array in one C call, and without
// this, every knob's on-screen value goes stale until the user happens to
// touch it — silently reverting whatever the preset just set the moment
// they do, since nudgeSlotLocked starts from the stale value. Also used
// right after plugin creation, since v2_create_instance auto-loads preset 0
// (if any presets exist) after defaultParams's forced set_param calls, and
// the same staleness applies to the very first frame drawn.
func (st *paramState) syncFromPluginState(plugin *C.bridge_plugin_t) {
	const bufLen = 8192
	buf := make([]byte, bufLen)
	key := C.CString("state")
	defer C.free(unsafe.Pointer(key))
	n := C.bridge_plugin_get_param(plugin, key, (*C.char)(unsafe.Pointer(&buf[0])), C.int(bufLen))
	if n < 0 {
		log.Printf("syncFromPluginState: get_param(state) failed")
		return
	}
	var values map[string]float64
	if err := json.Unmarshal(buf[:n], &values); err != nil {
		log.Printf("syncFromPluginState: parse state JSON: %v", err)
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	for key, val := range values {
		if slot, ok := st.slots[key]; ok {
			slot.value = val
		}
	}
	if presetSlot, ok := st.slots["preset"]; ok {
		st.presetCursor = int(presetSlot.value + 0.5)
	}
	st.dirty = true
}

// braidsPresetFile is the subset of a .braids preset JSON file this host
// reads — just enough to label the preset picker (see fetchPresetMeta).
type braidsPresetFile struct {
	Name string `json:"name"`
}

// fetchPresetMeta builds a synthetic enum paramMeta for "preset" — the
// plugin never lists it in chain_params (it's exposed only through its own
// ui_hierarchy browser convention, which this host doesn't use). Reading
// the .braids files directly off disk also avoids the alternative of
// cycling the live instance through every preset to read back its name:
// that would call v2_apply_preset for each one, overwriting the
// defaultParams values already sent to the instance by the time this runs.
func fetchPresetMeta(moduleDir string) (paramMeta, error) {
	dir := filepath.Join(moduleDir, "presets")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return paramMeta{}, fmt.Errorf("reading presets dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".braids") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files) // load_presets (braids_plugin.cpp) loads in this same sorted order
	names := make([]string, 0, len(files))
	for _, fn := range files {
		data, err := os.ReadFile(filepath.Join(dir, fn))
		if err != nil {
			return paramMeta{}, fmt.Errorf("reading %s: %w", fn, err)
		}
		var pf braidsPresetFile
		if err := json.Unmarshal(data, &pf); err != nil {
			return paramMeta{}, fmt.Errorf("parsing %s: %w", fn, err)
		}
		if pf.Name == "" {
			pf.Name = strings.TrimSuffix(fn, ".braids")
		}
		names = append(names, pf.Name)
	}
	if len(names) == 0 {
		return paramMeta{}, fmt.Errorf("no .braids presets found in %s", dir)
	}
	return paramMeta{
		Key:     "preset",
		Name:    "Preset",
		Type:    "enum",
		Min:     0,
		Max:     float64(len(names) - 1),
		Options: names,
	}, nil
}

// newParamState builds the page/slot state from the plugin's own metadata
// and defaultParams' starting values (see main.go's defaultParams) — every
// key named in paramPages that the plugin doesn't report is skipped with a
// log line rather than a crash, so a future Braids build that renames a
// param degrades to a shorter page instead of failing to start.
func newParamState(metas []paramMeta) *paramState {
	byKey := make(map[string]paramMeta, len(metas))
	for _, m := range metas {
		byKey[m.Key] = m
	}
	defaults := make(map[string]float64, len(defaultParams))
	for _, kv := range defaultParams {
		var v float64
		fmt.Sscanf(kv[1], "%f", &v)
		defaults[kv[0]] = v
	}

	st := &paramState{slots: make(map[string]*paramSlot)}
	addSlot := func(key string) {
		meta, ok := byKey[key]
		if !ok {
			log.Printf("params: %q not found in plugin's chain_params, skipping", key)
			return
		}
		st.slots[key] = &paramSlot{meta: meta, value: defaults[key]}
	}
	// allBankPages, not paramPages -- a param's slot must exist whether or
	// not its bank is the one currently on screen (see allBankPages' doc).
	for _, page := range allBankPages {
		for _, key := range page {
			addSlot(key)
		}
	}
	// "preset" and "octave_transpose" aren't on any paramPages grid page —
	// PRESETS (pagePresets) renders and drives them itself (see
	// renderPatchPage, movePresetCursor, loadStagedPreset, NudgeOctave).
	addSlot("preset")
	addSlot("octave_transpose")
	if presetSlot, ok := st.slots["preset"]; ok {
		st.presetCursor = int(presetSlot.value + 0.5)
	}
	return st
}

// stepFor is how much one encoder tick moves a param's value: exactly one
// unit for an enum/int (one shape, one semitone), or 1/100th of the
// param's full range for a float — full-range sweep in ~100 slow ticks,
// proportionally faster while the encoder is accelerating.
func stepFor(meta paramMeta) float64 {
	if meta.Type == "int" || meta.Type == "enum" {
		return 1
	}
	span := meta.Max - meta.Min
	if span <= 0 {
		span = 1
	}
	return span / 100.0
}

// nudgeSlotLocked applies one encoder's delta to slot (enum: accumulate to
// sensitivityFor(key) then step by one option; float/int: stepFor(meta)
// per tick), clamped to the plugin's own reported range. Caller must hold
// st.mu. Shared by applyEncoder (page-grid params) and NudgeOctave (a
// param outside any paramPages grid — see newParamState's addSlot doc).
func nudgeSlotLocked(slot *paramSlot, key string, delta int) (val string) {
	if slot.meta.Type == "enum" || slot.meta.Type == "int" {
		// Push's encoders accelerate — a fast turn sends delta up to ±11,
		// not ±1 (core/push3/encoder.go's DecodeRel doc). That's fine for
		// a continuous float sweep, but for a small discrete range it made
		// one brisk flick jump clean across it in a single message (an
		// enum's shape list, or octave_transpose's whole -3..3 span).
		// Accumulate delta instead and step by exactly one unit per
		// sensitivityFor(key) ticks of accumulated turn — 1 (the default)
		// steps on every message same as before a key was tuned; "engine"/
		// "octave_transpose" are heavier (see enumSensitivity) so browsing
		// them takes deliberate turning, not one graze.
		div := sensitivityFor(key)
		slot.accum += delta
		for slot.accum >= div {
			slot.value++
			slot.accum -= div
		}
		for slot.accum <= -div {
			slot.value--
			slot.accum += div
		}
	} else {
		slot.value += float64(delta) * stepFor(slot.meta)
	}
	if slot.value < slot.meta.Min {
		slot.value = slot.meta.Min
	}
	if slot.value > slot.meta.Max {
		slot.value = slot.meta.Max
	}
	if slot.meta.Type == "float" {
		return fmt.Sprintf("%.4f", slot.value)
	}
	return fmt.Sprintf("%d", int(slot.value+0.5))
}

// applyEncoder nudges the param in encoder slot idx (0-7) on the current
// paramPages grid page (pageXenia) by delta ticks, and returns
// the key/value string pair ready for bridge_plugin_set_param. ok is false
// when the current page isn't a paramPages grid page, or that encoder has
// no param on it.
func (st *paramState) applyEncoder(idx, delta int) (key, val string, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.page < 0 || st.page >= len(paramPages) {
		return "", "", false
	}
	page := paramPages[st.page]
	if idx < 0 || idx >= len(page) {
		return "", "", false
	}
	slot := st.slots[page[idx]]
	if slot == nil {
		return "", "", false
	}
	val = nudgeSlotLocked(slot, page[idx], delta)
	st.dirty = true
	return page[idx], val, true
}

// NudgeOctave applies one encoder tick to octave_transpose — PRESETS
// page's column-2 knob, immediate (unlike the staged preset browse in
// column 1) since it's not gated behind Load. Not on any paramPages grid
// page, so it can't go through applyEncoder.
func (st *paramState) NudgeOctave(delta int) (val string, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	slot := st.slots["octave_transpose"]
	if slot == nil {
		return "", false
	}
	val = nudgeSlotLocked(slot, "octave_transpose", delta)
	st.dirty = true
	return val, true
}

// NudgeMasterVolume applies one tick of Push3's dedicated hardware Volume
// encoder (push3.CCVolume, separate from the 8 param encoders) to
// channel_volume — not on any paramPages grid page, so it can't go
// through applyEncoder, and works the same on every page/bank.
func (st *paramState) NudgeMasterVolume(delta int) (val string, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	slot := st.slots["channel_volume"]
	if slot == nil {
		return "", false
	}
	val = nudgeSlotLocked(slot, "channel_volume", delta)
	st.dirty = true
	return val, true
}

// movePresetCursor moves PRESETS page's staged highlight by delta ticks
// (same accumulate-then-step feel as an enum param, via enumSensitivity's
// "preset" entry) — does not touch slots["preset"].value or the plugin;
// only loadStagedPreset (Load, bottom-1) does that.
func (st *paramState) movePresetCursor(delta int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	slot := st.slots["preset"]
	if slot == nil {
		return
	}
	div := sensitivityFor("preset")
	st.presetAccum += delta
	for st.presetAccum >= div {
		st.presetCursor++
		st.presetAccum -= div
	}
	for st.presetAccum <= -div {
		st.presetCursor--
		st.presetAccum += div
	}
	if n := len(slot.meta.Options); st.presetCursor >= n {
		st.presetCursor = n - 1
	}
	if st.presetCursor < 0 {
		st.presetCursor = 0
	}
	st.dirty = true
}

// PresetCursor returns PRESETS page's current staged highlight index.
func (st *paramState) PresetCursor() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.presetCursor
}

// loadStagedPreset commits the staged highlight as the actual preset
// (Load, bottom-1 on PRESETS) — the caller still owns sending it to the
// plugin (bridge_plugin_set_param("preset", idx)), same division of
// responsibility as applyEncoder/NudgeOctave.
func (st *paramState) loadStagedPreset() (idx int, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	slot := st.slots["preset"]
	if slot == nil {
		return 0, false
	}
	slot.value = float64(st.presetCursor)
	st.dirty = true
	return st.presetCursor, true
}

// setPage jumps directly to page n (a top-screen button press), clamped to
// the 4 fixed pages (see pageNames) — no relative D-Pad delta anymore.
func (st *paramState) setPage(n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if n < 0 {
		n = 0
	}
	if n > len(pageNames)-1 {
		n = len(pageNames) - 1
	}
	if n != st.page {
		st.page = n
		st.dirty = true
	}
}

// Page returns the current page index.
func (st *paramState) Page() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.page
}

// MarkDirty flags the display loop to redraw on its next tick — used by
// the SETTINGS page (iopage.go), which changes its own state outside of
// applyEncoder/setPage.
func (st *paramState) MarkDirty() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.dirty = true
}

// SetParam writes key's value directly, clamped to the plugin's own
// reported range — the web UI's absolute-write path (webserver.go),
// unlike nudgeSlotLocked's relative encoder ticks. Resets accum so a
// pending partial encoder turn doesn't compound with a web-set value.
// Returns the formatted value ready for bridge_plugin_set_param, same
// contract as applyEncoder/NudgeOctave. Setting "preset" this way also
// updates presetCursor, so PRESETS' on-screen highlight stays in sync with
// a preset picked from the browser.
func (st *paramState) SetParam(key string, val float64) (formatted string, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	slot := st.slots[key]
	if slot == nil {
		return "", false
	}
	if val < slot.meta.Min {
		val = slot.meta.Min
	}
	if val > slot.meta.Max {
		val = slot.meta.Max
	}
	slot.value = val
	slot.accum = 0
	if key == "preset" {
		st.presetCursor = int(val + 0.5)
	}
	st.dirty = true
	if slot.meta.Type == "float" {
		return fmt.Sprintf("%.4f", slot.value), true
	}
	return fmt.Sprintf("%d", int(slot.value+0.5)), true
}

// webParamPage is one on-screen page's title and param keys — the web UI's
// grouping (webserver.go's handleParams), reusing paramPages/pageNames so
// both surfaces present the same synth-style layout (OSC/AMP, FILTER,
// CRUSH/QUANT, AD/DRIFT) instead of the web UI inventing its own.
type webParamPage struct {
	Name string   `json:"name"`
	Keys []string `json:"keys"`
}

// webParamPages lists every param-grid page across BOTH banks (the
// browser has no D-Pad/bank concept, so it just shows everything at
// once) plus a synthetic PRESET page (preset + octave_transpose, which
// live outside paramPages — see newParamState's doc). pageSettings is
// skipped: it's I/O picker state (webserver.go's /api/io), not params.
// PRESETS/SETTINGS' nil entries in both banks are skipped by the same
// keys==nil check, so they naturally appear only once, not twice.
func webParamPages() []webParamPage {
	var out []webParamPage
	for b := range bankParamPages {
		for i, keys := range bankParamPages[b] {
			if keys == nil {
				continue
			}
			out = append(out, webParamPage{Name: bankPageNames[b][i], Keys: keys})
		}
	}
	out = append(out, webParamPage{Name: pageNames[pagePresets], Keys: []string{"preset", "octave_transpose"}})
	return out
}

// HasParam reports whether key names a known param — the web UI's request
// validation before queuing a ctlSetParam event (webserver.go).
func (st *paramState) HasParam(key string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.slots[key]
	return ok
}

// paramSnapshot is one param's live state, JSON-shaped for the web UI: its
// metadata plus current value and the same short display string the
// on-screen UI renders.
type paramSnapshot struct {
	Meta    paramMeta `json:"meta"`
	Value   float64   `json:"value"`
	Display string    `json:"display"`
}

// stateSnapshot is the web UI's GET /api/state and SSE payload shape.
type stateSnapshot struct {
	Page         int                      `json:"page"`
	PageName     string                   `json:"pageName"`
	PresetCursor int                      `json:"presetCursor"`
	Params       map[string]paramSnapshot `json:"params"`
}

// Snapshot returns every param's current value/meta plus the active page
// and staged preset cursor.
func (st *paramState) Snapshot() stateSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	params := make(map[string]paramSnapshot, len(st.slots))
	for key, slot := range st.slots {
		params[key] = paramSnapshot{Meta: slot.meta, Value: slot.value, Display: formatValue(slot)}
	}
	pageName := ""
	if st.page >= 0 && st.page < len(pageNames) {
		pageName = pageNames[st.page]
	}
	return stateSnapshot{Page: st.page, PageName: pageName, PresetCursor: st.presetCursor, Params: params}
}

// Metas returns every param's metadata, sorted by key — the web UI's
// one-shot GET /api/params (rarely changes, unlike Snapshot).
func (st *paramState) Metas() []paramMeta {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]paramMeta, 0, len(st.slots))
	for _, slot := range st.slots {
		out = append(out, slot.meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// formatValue renders a slot's current value as a short, human string: the
// enum's selected option name for "engine", a percentage for a plain 0-1
// float, or a plain number otherwise.
func formatValue(slot *paramSlot) string {
	m := slot.meta
	switch {
	case m.Type == "enum":
		i := int(slot.value + 0.5)
		if i >= 0 && i < len(m.Options) {
			return m.Options[i]
		}
		return fmt.Sprintf("%d", i)
	case m.Type == "float" && m.Min == 0 && m.Max == 1:
		return fmt.Sprintf("%d%%", int(slot.value*100+0.5))
	case m.Type == "int":
		return fmt.Sprintf("%d", int(slot.value+0.5))
	default:
		return fmt.Sprintf("%.3f", slot.value)
	}
}
