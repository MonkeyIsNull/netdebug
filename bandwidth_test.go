package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// goldenNetstatIB is a verbatim-style multi-interface `netstat -ibn` capture
// (all interfaces, including `*`-suffixed down markers and address-less <Link#N>
// rows). The en0 ADDRESS SUB-ROWS deliberately carry DIFFERENT decoy byte values
// than the en0 <Link#N> row, so a test asserting the Link totals actually proves
// the parser read the aggregate row and not a sub-row. An "en01" row is included
// for the prefix-collision case.
const goldenNetstatIB = `Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
lo0        16384 <Link#1>                      89427609     0 61731378109 89427609     0 61731378109     0
lo0        16384 127           127.0.0.1       89427609     - 61731378109 89427609     - 61731378109     -
lo0        16384 ::1/128     ::1               89427609     - 61731378109 89427609     - 61731378109     -
gif0*      1280  <Link#2>                             0     0          0        0     0          0     0
stf0*      1280  <Link#3>                             0     0          0        0     0          0     0
en0        1500  <Link#11>   02:00:5e:10:00:01 418534857     0 390516720444 284955761     0 261818858154     0
en0        1500  fe80::8a:5b fe80:b::8a:5b91:9        1     -          2        3     -          4     -
en0        1500  10/24         10.0.0.138             5     -          6        7     -          8     -
en01       1500  <Link#99>   02:00:5e:10:00:ff 111111111     0 222222222222 333333333     0 444444444444     0
utun0      1380  <Link#15>                            0     0          0      117     0      15777     0
utun0      1380  fe80::5d4a: fe80:f::5d4a:12e7        0     -          0      117     -      15777     -
`

func TestParseNetstatIB(t *testing.T) {
	tests := []struct {
		name  string
		out   string
		iface string
		want  IfaceCounters
	}{
		{
			name:  "en0 MAC-bearing link row read once, not the decoy sub-rows",
			out:   goldenNetstatIB,
			iface: "en0",
			want:  IfaceCounters{Name: "en0", RxBytes: 390516720444, TxBytes: 261818858154, OK: true},
		},
		{
			name:  "lo0 address-less 10-field link row (fails a fixed left-index parser)",
			out:   goldenNetstatIB,
			iface: "lo0",
			want:  IfaceCounters{Name: "lo0", RxBytes: 61731378109, TxBytes: 61731378109, OK: true},
		},
		{
			name:  "utun0 address-less asymmetric Ibytes=0 Obytes>0",
			out:   goldenNetstatIB,
			iface: "utun0",
			want:  IfaceCounters{Name: "utun0", RxBytes: 0, TxBytes: 15777, OK: true},
		},
		{
			name:  "gif0* down star-suffixed name matches gif0 and reports its counters",
			out:   goldenNetstatIB,
			iface: "gif0",
			want:  IfaceCounters{Name: "gif0", RxBytes: 0, TxBytes: 0, OK: true},
		},
		{
			name:  "lo0 present but en0 requested picks en0",
			out:   goldenNetstatIB,
			iface: "en0",
			want:  IfaceCounters{Name: "en0", RxBytes: 390516720444, TxBytes: 261818858154, OK: true},
		},
		{
			name:  "absent iface => OK=false zero counters",
			out:   goldenNetstatIB,
			iface: "en7",
			want:  IfaceCounters{Name: "en7"},
		},
		{
			name:  "empty string => OK=false",
			out:   "",
			iface: "en0",
			want:  IfaceCounters{Name: "en0"},
		},
		{
			name:  "header-only => OK=false",
			out:   "Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll\n",
			iface: "en0",
			want:  IfaceCounters{Name: "en0"},
		},
		{
			name:  "prefix collision: en0 requested never matches en01",
			out:   "en01       1500  <Link#99>   02:00:5e:10:00:ff 111111111     0 222222222222 333333333     0 444444444444     0\n",
			iface: "en0",
			want:  IfaceCounters{Name: "en0"},
		},
		{
			name:  "malformed: non-numeric Ibytes => OK=false",
			out:   "en0        1500  <Link#11>   02:00:5e:10:00:01 418534857     0 BADBYTES 284955761     0 261818858154     0\n",
			iface: "en0",
			want:  IfaceCounters{Name: "en0"},
		},
		{
			name:  "malformed: non-numeric Obytes => OK=false",
			out:   "en0        1500  <Link#11>   02:00:5e:10:00:01 418534857     0 390516720444 284955761     0 BADBYTES     0\n",
			iface: "en0",
			want:  IfaceCounters{Name: "en0"},
		},
		{
			name:  "address sub-row shape (trailing dashes) is not latched onto => OK=false",
			out:   "en0        1500  10/24         10.0.0.138      418534857     - 390516720444 284955761     - 261818858154     -\n",
			iface: "en0",
			want:  IfaceCounters{Name: "en0"},
		},
		{
			name:  "star-named row too short (<10 fields) => OK=false",
			out:   "en0*       1500  <Link#11>\n",
			iface: "en0",
			want:  IfaceCounters{Name: "en0"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNetstatIB(tc.out, tc.iface)
			if got != tc.want {
				t.Errorf("parseNetstatIB(%q) = %+v, want %+v", tc.iface, got, tc.want)
			}
		})
	}
}

func TestDeltaRate(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur uint64
		dt        float64
		wantBps   float64
		wantReset bool
	}{
		{"normal increase", 1000, 2000, 1, 1000, false},
		{"cur<prev reset leaks no huge value", 5e9, 100, 1, 0, true},
		{"wrap indistinguishable from reset", math.MaxUint64 - 10, 5, 1, 0, true},
		{"dt=0", 1000, 2000, 0, 0, false},
		{"dt below minDt", 1000, 2000, 0.0001, 0, false},
		{"no change", 2000, 2000, 1, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bps, reset := deltaRate(tc.prev, tc.cur, tc.dt)
			if reset != tc.wantReset {
				t.Fatalf("reset = %v, want %v", reset, tc.wantReset)
			}
			if bps != tc.wantBps {
				t.Errorf("bps = %v, want %v", bps, tc.wantBps)
			}
			if bps < 0 {
				t.Errorf("bps must never be negative, got %v", bps)
			}
		})
	}
}

func TestNewSample(t *testing.T) {
	t0 := time.Now()
	t1 := t0.Add(time.Second)

	t.Run("first sample prev.OK=false", func(t *testing.T) {
		cur := IfaceCounters{Name: "en0", RxBytes: 5000, TxBytes: 3000, OK: true}
		s := newSample(IfaceCounters{}, cur, time.Time{}, t1)
		if s.DownBytesPerSec != 0 || s.UpBytesPerSec != 0 || !s.Reset {
			t.Errorf("first sample: want zero rates + Reset=true, got %+v", s)
		}
		if s.RxBytes != 5000 || s.TxBytes != 3000 {
			t.Errorf("first sample raw bytes not populated from cur: %+v", s)
		}
	})

	t.Run("iface vanished cur.OK=false", func(t *testing.T) {
		prev := IfaceCounters{Name: "en0", RxBytes: 5000, TxBytes: 3000, OK: true}
		s := newSample(prev, IfaceCounters{Name: "en0"}, t0, t1)
		if s.DownBytesPerSec != 0 || s.UpBytesPerSec != 0 || !s.Reset {
			t.Errorf("vanished iface: want zero rates + Reset=true, got %+v", s)
		}
	})

	t.Run("second valid sample matches deltaRate", func(t *testing.T) {
		prev := IfaceCounters{Name: "en0", RxBytes: 1000, TxBytes: 500, OK: true}
		cur := IfaceCounters{Name: "en0", RxBytes: 3000, TxBytes: 1500, OK: true}
		s := newSample(prev, cur, t0, t1)
		if s.DownBytesPerSec != 2000 || s.UpBytesPerSec != 1000 || s.Reset {
			t.Errorf("want down=2000 up=1000 reset=false, got %+v", s)
		}
	})

	t.Run("dt==0 at newSample level", func(t *testing.T) {
		prev := IfaceCounters{Name: "en0", RxBytes: 1000, TxBytes: 500, OK: true}
		cur := IfaceCounters{Name: "en0", RxBytes: 3000, TxBytes: 1500, OK: true}
		s := newSample(prev, cur, t0, t0) // now == prevT
		if s.DownBytesPerSec != 0 || s.UpBytesPerSec != 0 {
			t.Errorf("dt==0: want zero rates, got %+v", s)
		}
	})

	t.Run("reset propagates cur<prev", func(t *testing.T) {
		prev := IfaceCounters{Name: "en0", RxBytes: 5_000_000_000, TxBytes: 500, OK: true}
		cur := IfaceCounters{Name: "en0", RxBytes: 100, TxBytes: 1500, OK: true}
		s := newSample(prev, cur, t0, t1)
		if !s.Reset {
			t.Errorf("want Reset=true on cur<prev, got %+v", s)
		}
		if s.DownBytesPerSec != 0 {
			t.Errorf("want down rate 0 on reset, got %v", s.DownBytesPerSec)
		}
	})
}

func TestNextBaseline(t *testing.T) {
	prev := IfaceCounters{Name: "en0", RxBytes: 1000, TxBytes: 500, OK: true}
	cur := IfaceCounters{Name: "en0", RxBytes: 2000, TxBytes: 900, OK: true}

	if got := nextBaseline(prev, cur); got != cur {
		t.Errorf("cur.OK=true: want cur %+v, got %+v", cur, got)
	}
	invalid := IfaceCounters{Name: "en0"}
	if got := nextBaseline(prev, invalid); got != prev {
		t.Errorf("cur.OK=false: want prev unchanged %+v, got %+v", prev, got)
	}
}

// TestDropRecoverNoPhantomSpike drives newSample+nextBaseline through a
// good -> invalid -> good sequence exactly as runSample would, and asserts the
// recovery step reports the real ~1s delta, not the whole cumulative counter.
func TestDropRecoverNoPhantomSpike(t *testing.T) {
	const base = uint64(390_000_000_000)
	const delta = uint64(1_250_000) // ~1 MB in the window

	t0 := time.Now()
	good1 := IfaceCounters{Name: "en0", RxBytes: base, TxBytes: base, OK: true}
	invalid := IfaceCounters{Name: "en0"} // OK=false
	good2 := IfaceCounters{Name: "en0", RxBytes: base + delta, TxBytes: base + delta, OK: true}

	// Sample 1 (warm-up): prev zero-value, OK=false.
	var prev IfaceCounters
	var prevT time.Time
	s1 := newSample(prev, good1, prevT, t0)
	if !s1.Reset {
		t.Fatalf("warm-up sample should be Reset=true, got %+v", s1)
	}
	prev = nextBaseline(prev, good1)
	prevT = t0

	// Sample 2: invalid read. Rates 0 / Reset=true, and the baseline must NOT
	// advance to the zeroed reading.
	t1 := t0.Add(time.Second)
	s2 := newSample(prev, invalid, prevT, t1)
	if !s2.Reset || s2.DownBytesPerSec != 0 || s2.UpBytesPerSec != 0 {
		t.Fatalf("invalid step: want zero rates + Reset=true, got %+v", s2)
	}
	prevAfter := nextBaseline(prev, invalid)
	if prevAfter != prev {
		t.Fatalf("baseline must not adopt a zeroed reading: got %+v", prevAfter)
	}
	// prevT stays put too (runSample only advances it on a valid read).

	// Sample 3: recovery. Diffs against the LAST GOOD baseline over ~2s, so the
	// rate is the real delta, NOT ~390 GB/s.
	t2 := t1.Add(time.Second)
	s3 := newSample(prevAfter, good2, prevT, t2)
	wantRate := float64(delta) / t2.Sub(prevT).Seconds()
	if s3.Reset {
		t.Errorf("recovery step should not be Reset, got %+v", s3)
	}
	if math.Abs(s3.DownBytesPerSec-wantRate) > 1 {
		t.Errorf("recovery down rate = %v, want ~%v (NOT the cumulative counter)", s3.DownBytesPerSec, wantRate)
	}
	if s3.DownBytesPerSec > float64(delta)*2 {
		t.Errorf("phantom spike: recovery rate %v is way above the real delta", s3.DownBytesPerSec)
	}
}

func TestRing(t *testing.T) {
	mk := func(n int) Sample { return Sample{RxBytes: uint64(n)} }

	t.Run("fewer than cap, oldest->newest", func(t *testing.T) {
		r := NewRing(5)
		for i := 1; i <= 3; i++ {
			r.Add(mk(i))
		}
		snap := r.Snapshot()
		if len(snap) != 3 || r.Len() != 3 {
			t.Fatalf("len = %d/%d, want 3", len(snap), r.Len())
		}
		for i, s := range snap {
			if s.RxBytes != uint64(i+1) {
				t.Errorf("snap[%d] = %d, want %d", i, s.RxBytes, i+1)
			}
		}
	})

	t.Run("exactly cap boundary", func(t *testing.T) {
		r := NewRing(3)
		for i := 1; i <= 3; i++ {
			r.Add(mk(i))
		}
		if r.Len() != 3 {
			t.Fatalf("Len = %d, want 3", r.Len())
		}
		snap := r.Snapshot()
		want := []uint64{1, 2, 3}
		for i, s := range snap {
			if s.RxBytes != want[i] {
				t.Errorf("snap[%d] = %d, want %d", i, s.RxBytes, want[i])
			}
		}
	})

	t.Run("more than cap keeps newest, slice cap never grows", func(t *testing.T) {
		r := NewRing(3)
		for i := 1; i <= 100; i++ {
			r.Add(mk(i))
			if c := cap(r.Snapshot()); c > 3 {
				t.Fatalf("snapshot backing cap grew to %d after %d Adds", c, i)
			}
		}
		if r.Len() != 3 {
			t.Fatalf("Len = %d, want 3", r.Len())
		}
		snap := r.Snapshot()
		want := []uint64{98, 99, 100}
		for i, s := range snap {
			if s.RxBytes != want[i] {
				t.Errorf("snap[%d] = %d, want %d", i, s.RxBytes, want[i])
			}
		}
	})

	t.Run("empty snapshot no panic", func(t *testing.T) {
		r := NewRing(4)
		if snap := r.Snapshot(); len(snap) != 0 {
			t.Errorf("empty ring snapshot len = %d, want 0", len(snap))
		}
	})

	t.Run("snapshot independence", func(t *testing.T) {
		r := NewRing(4)
		r.Add(mk(1))
		r.Add(mk(2))
		snap := r.Snapshot()
		snap[0].RxBytes = 999 // mutate the handed-off copy
		snap2 := r.Snapshot()
		if snap2[0].RxBytes != 1 {
			t.Errorf("ring data changed via returned slice: got %d, want 1", snap2[0].RxBytes)
		}
	})

	t.Run("NewRing(0)/NewRing(-1) no panic on Add", func(t *testing.T) {
		for _, c := range []int{0, -1} {
			r := NewRing(c)
			r.Add(mk(1))
			r.Add(mk(2)) // must not div-by-zero / panic
			if r.Len() != 1 {
				t.Errorf("NewRing(%d): Len = %d, want 1 (clamped cap)", c, r.Len())
			}
		}
	})
}

func TestHumanBps(t *testing.T) {
	tests := []struct {
		name        string
		bytesPerSec float64
		want        string
	}{
		{"zero", 0, "0 bps"},
		{"sub-Kbps bytes", 10, "80 bps"},     // 10 B/s * 8 = 80 bit/s
		{"~1e3 B/s => Kbps", 1000, "8 Kbps"}, // 8000 bit/s
		{"~1.5e6 B/s => Mbps", 1_500_000, "12.0 Mbps"},
		{">1.25e8 B/s => Gbps", 150_000_000, "1.2 Gbps"},
		{"just below Mbps threshold (999_999 bit/s)", 124999.875, "1000 Kbps"},
		{"exactly 1e6 bit/s => Mbps", 125000, "1.0 Mbps"},
		{"exactly 1e9 bit/s => Gbps", 125_000_000, "1.0 Gbps"},
		{"negative => 0 bps", -5, "0 bps"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanBps(tc.bytesPerSec); got != tc.want {
				t.Errorf("humanBps(%v) = %q, want %q", tc.bytesPerSec, got, tc.want)
			}
		})
	}

	// NaN / +Inf / -Inf must never crash or print garbage.
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := humanBps(v); got != "0 bps" {
			t.Errorf("humanBps(%v) = %q, want \"0 bps\"", v, got)
		}
	}
}

func TestClampInterval(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want time.Duration
	}{
		{100 * time.Millisecond, 250 * time.Millisecond},
		{250 * time.Millisecond, 250 * time.Millisecond},
		{time.Second, time.Second},
		{0, 250 * time.Millisecond},
		{-5 * time.Second, 250 * time.Millisecond},
	}
	for _, tc := range tests {
		if got := clampInterval(tc.in); got != tc.want {
			t.Errorf("clampInterval(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestSampleJSONShape pins the Phase 2 wire schema: Reset=false omits the "reset"
// key (reset,omitempty, paralleling TestDefaultJSONHasNoOptInStanzas) and the
// per-sec keys are down_bytes_per_sec / up_bytes_per_sec (bytes, not bits).
func TestSampleJSONShape(t *testing.T) {
	s := Sample{
		T:               time.Unix(0, 0).UTC(),
		RxBytes:         1000,
		TxBytes:         500,
		DownBytesPerSec: 2000,
		UpBytesPerSec:   1000,
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, key := range []string{`"rx_bytes"`, `"tx_bytes"`, `"down_bytes_per_sec"`, `"up_bytes_per_sec"`} {
		if !strings.Contains(js, key) {
			t.Errorf("Sample JSON missing %s: %s", key, js)
		}
	}
	if strings.Contains(js, `"reset"`) {
		t.Errorf("Reset=false should omit the reset key: %s", js)
	}

	withReset, _ := json.Marshal(Sample{Reset: true})
	if !strings.Contains(string(withReset), `"reset":true`) {
		t.Errorf("Reset=true should emit the reset key: %s", withReset)
	}
}

// TestSampleLinkWireShape pins the Phase-4 backward-compat guarantee: a nil Link
// (pointer omitempty) emits NO "link" key, so warm-up and --sample samples keep
// the exact Phase-2 wire; a non-nil Link (even OK=false) emits "link" with
// "link_ok":false. Nothing else today asserts the ABSENCE of the key, so this is
// the regression catcher for the "/data.json consumers must not break" lock.
func TestSampleLinkWireShape(t *testing.T) {
	nilLink, err := json.Marshal(Sample{T: time.Unix(0, 0).UTC(), RxBytes: 1, TxBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(nilLink), `"link"`) {
		t.Errorf("Link=nil must omit the link key entirely: %s", nilLink)
	}

	withLink, err := json.Marshal(Sample{T: time.Unix(0, 0).UTC(), Link: &LinkSnap{OK: false}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(withLink)
	if !strings.Contains(s, `"link"`) {
		t.Errorf("non-nil Link must emit the link key: %s", s)
	}
	if !strings.Contains(s, `"link_ok":false`) {
		t.Errorf("link_ok must serialize even when false (not omitempty): %s", s)
	}
	// Phase 6: a down/warm-up snapshot omits ALL the new keys (additive, no bloat).
	for _, k := range []string{`"noise"`, `"snr"`, `"tx_rate_mbps"`, `"mcs"`, `"phy"`} {
		if strings.Contains(s, k) {
			t.Errorf("OK=false snapshot must omit new key %s: %s", k, s)
		}
	}

	// A Phase-4-shaped link (only band/ch/rssi set, new fields zero/nil) still emits
	// NO new keys — strictly additive, no renamed/removed keys — and round-trips.
	full, _ := json.Marshal(Sample{Link: &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", RSSI: -43, OK: true}})
	fs := string(full)
	for _, k := range []string{`"noise"`, `"snr"`, `"tx_rate_mbps"`, `"mcs"`, `"phy"`} {
		if strings.Contains(fs, k) {
			t.Errorf("Phase-4-shaped link must omit new key %s: %s", k, fs)
		}
	}
	var back Sample
	if err := json.Unmarshal(full, &back); err != nil {
		t.Fatal(err)
	}
	if back.Link == nil || back.Link.Band != "5 GHz" || back.Link.Channel != "149/80" || back.Link.RSSI != -43 || !back.Link.OK {
		t.Errorf("link round-trip lost data: %+v", back.Link)
	}

	// MCS pointer presence: a genuine MCS 0 (non-nil &0) emits "mcs":0; a nil MCS
	// omits the key. This is the whole point of the *int — int+omitempty would drop
	// a real MCS 0 and conflate it with "absent".
	zero := 0
	mcs0, _ := json.Marshal(Sample{Link: &LinkSnap{OK: true, MCS: &zero}})
	if !strings.Contains(string(mcs0), `"mcs":0`) {
		t.Errorf("MCS=&0 must serialize mcs:0 (present): %s", mcs0)
	}
	mcsNil, _ := json.Marshal(Sample{Link: &LinkSnap{OK: true, MCS: nil}})
	if strings.Contains(string(mcsNil), `"mcs"`) {
		t.Errorf("MCS=nil must omit the mcs key: %s", mcsNil)
	}

	// A fully-populated modern snapshot round-trips with every new field intact.
	eleven := 11
	modern, _ := json.Marshal(Sample{Link: &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: -92, SNR: 49, TxRate: 1200, MCS: &eleven, PHY: "802.11ax", OK: true}})
	var mback Sample
	if err := json.Unmarshal(modern, &mback); err != nil {
		t.Fatal(err)
	}
	if mback.Link == nil || mback.Link.Noise != -92 || mback.Link.SNR != 49 || mback.Link.TxRate != 1200 ||
		mback.Link.PHY != "802.11ax" || mback.Link.MCS == nil || *mback.Link.MCS != 11 {
		t.Errorf("modern link round-trip lost data: %+v (MCS=%v)", mback.Link, mback.Link.MCS)
	}
}
