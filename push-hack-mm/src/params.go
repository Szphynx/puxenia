package main

// params.go — parameter metadata and live value/page state for the
// on-screen control UI (see display.go) and the 8 encoders + D-Pad
// Left/Right (page bank) + D-Pad Up/Down (track select) that drive it.
// Adapted from push-hack-xenia/src/params.go's shape; the real structural
// difference is that every param here is *per track* (Monomachine has 6
// independent synth tracks, mdLib/mdautomation.h's monomachine::TrackCount)
// — paramState itself stays single-valued per key (same as Xenia), but
// represents "the currently selected track's" values, and gets resynced
// via syncFromPluginState every time the track changes (see mm_plugin.cpp's
// get_param("state"), which answers for whichever track is currently
// selected).
//
// Metadata (name/type/min/max) comes from the DSP plugin's own
// get_param("chain_params") — see mm_plugin.cpp's mm_get_param, built
// straight from parameterDescriptions_mm.json's real page/index layout, so
// this host never hardcodes Monomachine-specific ranges.

/*
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"unsafe"
)

type paramMeta struct {
	Key     string   `json:"key"`
	Name    string   `json:"name"`
	Type    string   `json:"type"` // "int" or "enum" — mm_plugin.cpp never emits "float"
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
	Options []string `json:"options"`
}

// pageSeq/pageSettings are the 2 special pages, pinned in both banks
// (slots 6/7) same as Xenia's PRESETS/SETTINGS — see bankPageNames' doc.
const (
	pageSeq      = 6
	pageSettings = 7
)

// bankPageNames/bankParamPages: 7 real Monomachine per-track pages
// (Synthesis/Amplification/Filter/Effects/LFO1-3) plus a combined
// LEVEL/MUTE page — 8 content pages for Push's 8 top-screen buttons, one
// too many to also fit SEQ+SETTINGS, so (same fix as Xenia) 2 banks of 8,
// flipped with D-Pad Left/Right, SEQ/SETTINGS pinned to slots 6/7 in both.
var bankPageNames = [2][8]string{
	{"SYNTH", "AMP", "FILTER", "EFFECTS", "LFO 1", "LFO 2", "SEQ", "SETTINGS"},
	{"LFO 3", "LEVEL", "", "", "", "", "SEQ", "SETTINGS"},
}

var bankParamPages = [2][8][]string{
	{
		{"synth_a", "synth_b", "synth_c", "synth_d", "synth_e", "synth_f", "synth_g", "synth_h"},
		{"amp_attack", "amp_hold", "amp_decay", "amp_release", "amp_distortion", "amp_volume", "amp_pan", "amp_portamento"},
		{"filter_base", "filter_width", "filter_hp_q", "filter_lp_q", "filter_attack", "filter_decay", "filter_base_offset", "filter_width_offset"},
		{"fx_eq_freq", "fx_eq_gain", "fx_srr", "fx_delay_time", "fx_delay_send", "fx_delay_feedback", "fx_delay_base", "fx_delay_width"},
		{"lfo1_page", "lfo1_dest", "lfo1_trig", "lfo1_wave", "lfo1_mult", "lfo1_speed", "lfo1_interlace", "lfo1_depth"},
		{"lfo2_page", "lfo2_dest", "lfo2_trig", "lfo2_wave", "lfo2_mult", "lfo2_speed", "lfo2_interlace", "lfo2_depth"},
		nil, // SEQ
		nil, // SETTINGS
	},
	{
		{"lfo3_page", "lfo3_dest", "lfo3_trig", "lfo3_wave", "lfo3_mult", "lfo3_speed", "lfo3_interlace", "lfo3_depth"},
		{"level", "mute"},
		nil, // spare
		nil, // spare
		nil, // spare
		nil, // spare
		nil, // SEQ
		nil, // SETTINGS
	},
}

var currentBank = 0

var (
	paramPages = bankParamPages[0][:]
	pageNames  = bankPageNames[0][:]
)

// setBank flips to bank 0 or 1 (any other value is a no-op) and jumps to
// that bank's first page — same shape as Xenia's setBank.
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

var allBankPages = append(append([][]string{}, bankParamPages[0][:]...), bankParamPages[1][:]...)

type paramSlot struct {
	meta  paramMeta
	value float64
	accum int
}

// enumSensitivity: "track" is the only enum-like slot this host nudges
// via an accelerating encoder anywhere (SETTINGS' BASE CHANNEL column
// uses ioState's own plain +1/-1 cursor instead, same as Xenia's I/O
// picker columns) — heavier than 1 so a fast turn doesn't blow past
// several tracks in one message, same reasoning as Xenia's "program".
var enumSensitivity = map[string]int{
	"track": 3,
}

func sensitivityFor(key string) int {
	if d, ok := enumSensitivity[key]; ok && d > 0 {
		return d
	}
	return 1
}

// paramState guards the current page and every param's value (for the
// CURRENTLY SELECTED TRACK — see this file's header doc) against
// concurrent access: the render loop goroutine (main.go) writes it on
// every encoder/page/track event, the display loop goroutine (display.go)
// reads it at ~10fps to redraw, and webserver.go's SSE broadcaster reads
// it every 100ms.
type paramState struct {
	mu    sync.Mutex
	page  int
	slots map[string]*paramSlot
	dirty bool
}

// fetchChainParams calls the plugin's get_param("chain_params") and parses
// the resulting JSON metadata list — see mm_plugin.cpp's mm_get_param.
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
	return metas, nil
}

// trackMeta overrides "track"'s bare int metadata (0-5) from chain_params
// with named options ("Track 1".."Track 6") — nicer for the SEQ page's
// header and the web UI's track picker than a raw number. Called once
// from newParamState right after fetchChainParams.
func trackMeta(metas []paramMeta) paramMeta {
	for _, m := range metas {
		if m.Key == "track" {
			names := make([]string, int(m.Max)+1)
			for i := range names {
				names[i] = fmt.Sprintf("Track %d", i+1)
			}
			return paramMeta{Key: "track", Name: "Track", Type: "enum", Min: m.Min, Max: m.Max, Options: names}
		}
	}
	// Fallback, should never trigger since mm_plugin.cpp always reports
	// "track" — keeps newParamState from panicking on a lookup miss if a
	// future plugin build ever drops it.
	names := make([]string, mmNumTracks)
	for i := range names {
		names[i] = fmt.Sprintf("Track %d", i+1)
	}
	return paramMeta{Key: "track", Name: "Track", Type: "enum", Min: 0, Max: float64(mmNumTracks - 1), Options: names}
}

// mmNumTracks — Monomachine's fixed track count (mdLib/mdautomation.h's
// monomachine::TrackCount). Duplicated here as a plain Go constant rather
// than round-tripped through the plugin boundary a second time, since
// main.go's pad-grid row mapping also needs it directly.
const mmNumTracks = 6

// syncFromPluginState re-reads the plugin's get_param("state") (the
// CURRENTLY SELECTED track's own shadow values — see mm_plugin.cpp) and
// overwrites every matching slot's value with it. Called right after
// plugin creation, AND every time the track changes (main.go/
// audiosession.go's track-select paths) — without the latter, switching
// tracks would show the previous track's slider values until the user
// happened to touch one, silently writing it to the wrong track.
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
	st.dirty = true
}

// newParamState builds the page/slot state from the plugin's own metadata
// — every key named in paramPages that the plugin doesn't report is
// skipped with a log line rather than a crash.
func newParamState(metas []paramMeta) *paramState {
	byKey := make(map[string]paramMeta, len(metas))
	for _, m := range metas {
		byKey[m.Key] = m
	}
	byKey["track"] = trackMeta(metas)

	st := &paramState{slots: make(map[string]*paramSlot)}
	addSlot := func(key string) {
		meta, ok := byKey[key]
		if !ok {
			log.Printf("params: %q not found in plugin's chain_params, skipping", key)
			return
		}
		st.slots[key] = &paramSlot{meta: meta, value: meta.Min}
	}
	for _, page := range allBankPages {
		for _, key := range page {
			addSlot(key)
		}
	}
	// "track" lives outside any paramPages grid page — SEQ (pageSeq) and
	// the D-Pad Up/Down handler read/drive it directly (see main.go).
	addSlot("track")
	return st
}

func stepFor(meta paramMeta) float64 {
	// Every mm_plugin.cpp param is "int" (0-127 CC-range values) —
	// int/enum slots step by 1 per encoder tick, same as Xenia's int path.
	return 1
}

func nudgeSlotLocked(slot *paramSlot, key string, delta int) (val string) {
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
	if slot.value < slot.meta.Min {
		slot.value = slot.meta.Min
	}
	if slot.value > slot.meta.Max {
		slot.value = slot.meta.Max
	}
	return fmt.Sprintf("%d", int(slot.value+0.5))
}

// applyEncoder nudges the param in encoder slot idx (0-7) on the current
// paramPages grid page by delta ticks, and returns the key/value string
// pair ready for bridge_plugin_set_param. ok is false when the current
// page isn't a paramPages grid page, or that encoder has no param on it.
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

// NudgeTrack applies one D-Pad Up/Down press to the "track" slot (+1/-1,
// clamped — no wraparound, matching how a physical Track button strip
// would behave). Returns the new track index and the formatted value
// ready for bridge_plugin_set_param("track", ...). Works on every
// page/bank, same as Xenia's NudgeMasterVolume.
func (st *paramState) NudgeTrack(delta int) (val string, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	slot := st.slots["track"]
	if slot == nil {
		return "", false
	}
	val = nudgeSlotLocked(slot, "track", delta)
	st.dirty = true
	return val, true
}

// CurrentTrack returns the on-screen "track" slot's live value (0-5) —
// read by main.go's pad handler to decide which channel a live-played
// note routes to, and by display.go/leds.go to highlight the active row.
func (st *paramState) CurrentTrack() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	slot := st.slots["track"]
	if slot == nil {
		return 0
	}
	return int(slot.value + 0.5)
}

// setPage jumps directly to page n (a top-screen button press), clamped
// to the fixed pages.
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

func (st *paramState) Page() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.page
}

func (st *paramState) MarkDirty() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.dirty = true
}

// SetParam writes key's value directly, clamped to the plugin's own
// reported range — the web UI's absolute-write path (webserver.go).
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
	st.dirty = true
	return fmt.Sprintf("%d", int(slot.value+0.5)), true
}

type webParamPage struct {
	Name string   `json:"name"`
	Keys []string `json:"keys"`
}

// webParamPages lists every param-grid page across BOTH banks (the
// browser has no D-Pad/bank concept, so it just shows everything at once).
// SEQ/SETTINGS are skipped: SEQ is the step grid (webserver.go's own
// /api/seq/* endpoints), SETTINGS is I/O picker state (/api/io).
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
	return out
}

func (st *paramState) HasParam(key string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.slots[key]
	return ok
}

type paramSnapshot struct {
	Meta    paramMeta `json:"meta"`
	Value   float64   `json:"value"`
	Display string    `json:"display"`
}

type stateSnapshot struct {
	Page     int                      `json:"page"`
	PageName string                   `json:"pageName"`
	Track    int                      `json:"track"`
	Params   map[string]paramSnapshot `json:"params"`
}

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
	track := 0
	if slot, ok := st.slots["track"]; ok {
		track = int(slot.value + 0.5)
	}
	return stateSnapshot{Page: st.page, PageName: pageName, Track: track, Params: params}
}

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

func formatValue(slot *paramSlot) string {
	m := slot.meta
	switch {
	case m.Type == "enum":
		i := int(slot.value + 0.5)
		if i >= 0 && i < len(m.Options) {
			return m.Options[i]
		}
		return fmt.Sprintf("%d", i)
	case slot.meta.Key == "mute":
		if slot.value >= 0.5 {
			return "ON"
		}
		return "OFF"
	default:
		return fmt.Sprintf("%d", int(slot.value+0.5))
	}
}
