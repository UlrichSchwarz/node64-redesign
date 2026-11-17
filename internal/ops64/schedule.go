package ops64

import (
	"errors"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Zeitplaene der Hosts lesen: cron (fuenf Felder bzw. @daily ...) und Kalender-Angaben, wie systemd (OnCalendar) und
// Proxmox (Backup- und PBS-Jobs) sie schreiben. Gerechnet wird auf die Minute; Sekunden zaehlen nicht. Was sich nicht
// lesen laesst (z. B. "~" fuer die letzten Tage eines Monats, feste Jahre), ist ein Fehler: die Aufgabe steht dann in
// der Liste, aber nicht im Zeitplan.

// Syntax eines Zeitplans.
const (
	SyntaxCron     = "cron"
	SyntaxCalendar = "calendar"
)

var (
	// ErrSchedule: Zeitplan nicht lesbar.
	ErrSchedule = errors.New("ops64: schedule not understood")
)

// spec ist eine Angabe: Minuten, Stunden, Tage des Monats, Monate, Wochentage (Bit 0 = Sonntag).
type spec struct {
	min    uint64
	hour   uint32
	dom    uint32 // Bits 1..31
	month  uint16 // Bits 1..12
	dow    uint8
	cronOr bool // cron: Tag des Monats ODER Wochentag, wenn beide eingeschraenkt sind
}

// Schedule ist ein gelesener Zeitplan (eine oder mehrere Angaben).
type Schedule struct {
	specs []spec
	never bool // z. B. @reboot: kein fester Zeitpunkt
}

const (
	allHour  = uint32(1)<<24 - 1
	allDom   = ^uint32(1)
	allMonth = uint16(1)<<13 - 2
	allDow   = uint8(1)<<7 - 1
)

// ParseSchedule liest einen Zeitplan. syntax leer: wird erkannt. Mehrere Angaben trennt ";" oder ein Zeilenumbruch.
func ParseSchedule(syntax, text string) (*Schedule, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrSchedule
	}
	out := &Schedule{}
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ';' || r == '\n' }) {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		syn := syntax
		if syn == "" {
			syn = guessSyntax(part)
		}
		var (
			sp  spec
			err error
		)
		switch syn {
		case SyntaxCron:
			if part == "@reboot" {
				out.never = true
				continue
			}
			sp, err = parseCron(part)
		case SyntaxCalendar:
			sp, err = parseCalendar(part)
		default:
			err = ErrSchedule
		}
		if err != nil {
			return nil, err
		}
		out.specs = append(out.specs, sp)
	}
	if len(out.specs) == 0 && !out.never {
		return nil, ErrSchedule
	}
	return out, nil
}

// guessSyntax: fuenf Felder ohne Doppelpunkt bzw. "@..." sind cron, alles andere eine Kalender-Angabe.
func guessSyntax(s string) string {
	if strings.HasPrefix(s, "@") {
		return SyntaxCron
	}
	if len(strings.Fields(s)) == 5 && !strings.ContainsAny(s, ":") && !strings.Contains(s, "..") {
		return SyntaxCron
	}
	return SyntaxCalendar
}

var cronAlias = map[string]string{"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *", "@weekly": "0 0 * * 0",
	"@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *"}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dowNames   = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
)

func parseCron(s string) (spec, error) {
	if a, ok := cronAlias[s]; ok {
		s = a
	}
	f := strings.Fields(s)
	if len(f) != 5 {
		return spec{}, ErrSchedule
	}
	minute, err := parseField(f[0], 0, 59, nil, "-")
	if err != nil {
		return spec{}, err
	}
	hour, err := parseField(f[1], 0, 23, nil, "-")
	if err != nil {
		return spec{}, err
	}
	dom, err := parseField(f[2], 1, 31, nil, "-")
	if err != nil {
		return spec{}, err
	}
	month, err := parseField(f[3], 1, 12, monthNames, "-")
	if err != nil {
		return spec{}, err
	}
	dow, err := parseField(f[4], 0, 7, dowNames, "-")
	if err != nil {
		return spec{}, err
	}
	if dow&(1<<7) != 0 { // 7 = Sonntag
		dow = dow&^(1<<7) | 1
	}
	sp := spec{min: minute, hour: uint32(hour), dom: uint32(dom), month: uint16(month), dow: uint8(dow)}
	sp.cronOr = !strings.HasPrefix(f[2], "*") && !strings.HasPrefix(f[4], "*")
	return sp, nil
}

// parseField liest ein Feld als Bitmenge: "*", "*/n", "a", "a-b" bzw. "a..b", "a-b/n", "a/n" und Listen mit Komma.
// names: Namen statt Zahlen (Monate, Wochentage); rng ist das Zeichen fuer Bereiche neben "..".
func parseField(s string, lo, hi int, names map[string]int, rng string) (uint64, error) {
	var out uint64
	if s == "" {
		return 0, ErrSchedule
	}
	for _, part := range strings.Split(s, ",") {
		step := 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 || n > hi {
				return 0, ErrSchedule
			}
			step, part = n, part[:i]
			if part != "*" && !strings.Contains(part, "..") && (rng == "" || !strings.Contains(part, rng)) {
				part += ".." + strconv.Itoa(hi) // "a/n": ab a bis zum Ende
			}
		}
		from, to := lo, hi
		if part != "*" {
			a, b, isRange := part, "", false
			if i := strings.Index(part, ".."); i >= 0 {
				a, b, isRange = part[:i], part[i+2:], true
			} else if rng != "" {
				if i := strings.Index(part, rng); i > 0 {
					a, b, isRange = part[:i], part[i+len(rng):], true
				}
			}
			var err error
			if from, err = fieldValue(a, names); err != nil {
				return 0, err
			}
			to = from
			if isRange {
				if to, err = fieldValue(b, names); err != nil {
					return 0, err
				}
			}
		}
		if from < lo || to > hi || from > to {
			return 0, ErrSchedule
		}
		for v := from; v <= to; v += step {
			out |= 1 << uint(v)
		}
	}
	return out, nil
}

func fieldValue(s string, names map[string]int) (int, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	if len(s) >= 3 {
		if n, ok := names[s[:3]]; ok {
			return n, nil
		}
	}
	return 0, ErrSchedule
}

var calAlias = map[string]string{"minutely": "*:*", "hourly": "*:00", "daily": "00:00", "weekly": "mon 00:00", "monthly": "*-*-01 00:00",
	"yearly": "*-01-01 00:00", "annually": "*-01-01 00:00", "quarterly": "*-01,04,07,10-01 00:00", "semiannually": "*-01,07-01 00:00",
	"semi-annually": "*-01,07-01 00:00"}

// parseCalendar liest "[Wochentage] [Datum] [Zeit]" (systemd und Proxmox): "mon..fri 02:00", "*-*-01 04:00:00",
// "sat 03:30", "2,22:30", "*/30" (Proxmox: nur Minuten), "daily".
func parseCalendar(s string) (spec, error) {
	if a, ok := calAlias[s]; ok {
		s = a
	}
	sp := spec{min: 1, hour: 1, dom: allDom, month: allMonth, dow: allDow}
	var seenDow, seenDate, seenTime bool
	for _, tok := range strings.Fields(s) {
		switch {
		case tok[0] >= 'a' && tok[0] <= 'z':
			if seenDow || seenDate || seenTime {
				return spec{}, ErrSchedule
			}
			d, err := parseDays(tok)
			if err != nil {
				return spec{}, err
			}
			sp.dow, seenDow = d, true
		case strings.Contains(tok, ":"):
			if seenTime {
				return spec{}, ErrSchedule
			}
			f := strings.Split(tok, ":")
			if len(f) < 2 || len(f) > 3 {
				return spec{}, ErrSchedule
			}
			h, err := parseField(f[0], 0, 23, nil, "")
			if err != nil {
				return spec{}, err
			}
			m, err := parseField(f[1], 0, 59, nil, "")
			if err != nil {
				return spec{}, err
			}
			if len(f) == 3 { // Sekunden nur pruefen
				if _, err := parseField(f[2], 0, 59, nil, ""); err != nil {
					return spec{}, err
				}
			}
			sp.hour, sp.min, seenTime = uint32(h), m, true
		case strings.Contains(tok, "-"):
			if seenDate || seenTime {
				return spec{}, ErrSchedule
			}
			f := strings.Split(tok, "-")
			if len(f) == 3 {
				if f[0] != "*" { // feste Jahre: nicht planbar
					return spec{}, ErrSchedule
				}
				f = f[1:]
			}
			if len(f) != 2 {
				return spec{}, ErrSchedule
			}
			mo, err := parseField(f[0], 1, 12, nil, "")
			if err != nil {
				return spec{}, err
			}
			d, err := parseField(f[1], 1, 31, nil, "")
			if err != nil {
				return spec{}, err
			}
			sp.month, sp.dom, seenDate = uint16(mo), uint32(d), true
		default: // Proxmox: nur Minuten, jede Stunde
			if seenTime {
				return spec{}, ErrSchedule
			}
			m, err := parseField(tok, 0, 59, nil, "")
			if err != nil {
				return spec{}, err
			}
			sp.hour, sp.min, seenTime = allHour, m, true
		}
	}
	if !seenDow && !seenDate && !seenTime {
		return spec{}, ErrSchedule
	}
	return sp, nil
}

// parseDays liest Wochentage: "mon", "mon..fri", "mon-fri", "sat,sun" (auch ausgeschrieben).
func parseDays(s string) (uint8, error) {
	var out uint8
	for _, part := range strings.Split(s, ",") {
		a, b := part, part
		if i := strings.Index(part, ".."); i >= 0 {
			a, b = part[:i], part[i+2:]
		} else if i := strings.IndexByte(part, '-'); i >= 0 {
			a, b = part[:i], part[i+1:]
		}
		from, err := fieldValue(a, dowNames)
		if err != nil || (a[0] >= '0' && a[0] <= '9') {
			return 0, ErrSchedule
		}
		to, err := fieldValue(b, dowNames)
		if err != nil || (b[0] >= '0' && b[0] <= '9') {
			return 0, ErrSchedule
		}
		for d := from; ; d = (d + 1) % 7 { // "sat..mon" laeuft ueber den Sonntag
			out |= 1 << uint(d)
			if d == to {
				break
			}
		}
	}
	return out, nil
}

func (sp spec) day(t time.Time) bool {
	if sp.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	dom, dow := sp.dom&(1<<uint(t.Day())) != 0, sp.dow&(1<<uint(t.Weekday())) != 0
	if sp.cronOr {
		return dom || dow
	}
	return dom && dow
}

// Never: der Zeitplan hat keine festen Zeitpunkte (z. B. @reboot).
func (s *Schedule) Never() bool { return s == nil || len(s.specs) == 0 }

// Between liefert die Zeitpunkte in [from, to) in der Zeitzone loc, aufsteigend, hoechstens limit (0 = alle).
func (s *Schedule) Between(from, to time.Time, loc *time.Location, limit int) []time.Time {
	if s.Never() || !from.Before(to) {
		return nil
	}
	var out []time.Time
	from, to = from.In(loc), to.In(loc)
	y, mo, d := from.Date()
	for day := time.Date(y, mo, d, 12, 0, 0, 0, loc); ; day = day.AddDate(0, 0, 1) { // mittags: kein Aerger mit der Zeitumstellung
		if y, mo, d = day.Date(); !time.Date(y, mo, d, 0, 0, 0, 0, loc).Before(to) {
			break
		}
		n := len(out)
		for _, sp := range s.specs {
			if !sp.day(day) {
				continue
			}
			for h := 0; h < 24; h++ {
				if sp.hour&(1<<uint(h)) == 0 {
					continue
				}
				for m := 0; m < 60; m++ {
					if sp.min&(1<<uint(m)) == 0 {
						continue
					}
					t := time.Date(y, mo, d, h, m, 0, 0, loc)
					if t.Hour() != h || t.Before(from) || !t.Before(to) { // Stunde gibt es an diesem Tag nicht (Sommerzeit)
						continue
					}
					out = append(out, t)
				}
			}
		}
		if len(s.specs) > 1 {
			sort.Slice(out[n:], func(i, j int) bool { return out[n+i].Before(out[n+j]) })
			out = dedupTimes(out, n)
		}
		if limit > 0 && len(out) >= limit {
			return out[:limit]
		}
	}
	return out
}

func dedupTimes(ts []time.Time, from int) []time.Time {
	out := ts[:from]
	for i := from; i < len(ts); i++ {
		if len(out) > from && out[len(out)-1].Equal(ts[i]) {
			continue
		}
		out = append(out, ts[i])
	}
	return out
}

// Next ist der naechste Zeitpunkt nach t (Null = keiner innerhalb von zwei Jahren).
func (s *Schedule) Next(t time.Time, loc *time.Location) time.Time {
	from := t.Truncate(time.Minute).Add(time.Minute)
	if l := s.Between(from, from.AddDate(2, 0, 0), loc, 1); len(l) > 0 {
		return l[0]
	}
	return time.Time{}
}

// Prev ist der letzte Zeitpunkt bis einschliesslich t (Null = keiner in den letzten 62 Tagen).
func (s *Schedule) Prev(t time.Time, loc *time.Location) time.Time {
	for back := 1; back <= 62; back *= 2 {
		if l := s.Between(t.AddDate(0, 0, -back), t.Add(time.Second), loc, 0); len(l) > 0 {
			return l[len(l)-1]
		}
	}
	return time.Time{}
}

// PerDay: Zeitpunkte an einem Tag, an dem der Zeitplan gilt (hoechster Wert der Angaben).
func (s *Schedule) PerDay() int {
	n := 0
	if s == nil {
		return 0
	}
	for _, sp := range s.specs {
		n = max(n, bits.OnesCount32(sp.hour)*bits.OnesCount64(sp.min))
	}
	return n
}

// Summary beschreibt einfache Zeitplaene fuer die Anzeige. kind: daily | days | monthly | hourly | minutes | "" (nur
// der Text der Quelle); clock "HH:MM"; days Wochentage (0 = Sonntag); n Tag des Monats, Minute bzw. Abstand.
func (s *Schedule) Summary() (kind, clock string, days []int, n int) {
	if s == nil || len(s.specs) != 1 {
		return "", "", nil, 0
	}
	sp := s.specs[0]
	allDays := sp.dom == allDom && sp.month == allMonth && sp.dow == allDow
	oneMin, oneHour := bits.OnesCount64(sp.min) == 1, bits.OnesCount32(sp.hour) == 1
	if oneMin && oneHour {
		clock = two(bits.TrailingZeros32(sp.hour)) + ":" + two(bits.TrailingZeros64(sp.min))
		switch {
		case allDays:
			return "daily", clock, nil, 0
		case sp.dom == allDom && sp.month == allMonth:
			for d := 1; d <= 7; d++ { // Montag zuerst
				if sp.dow&(1<<uint(d%7)) != 0 {
					days = append(days, d%7)
				}
			}
			return "days", clock, days, 0
		case sp.dow == allDow && sp.month == allMonth && bits.OnesCount32(sp.dom) == 1:
			return "monthly", clock, nil, bits.TrailingZeros32(sp.dom)
		}
		return "", "", nil, 0
	}
	if !allDays || sp.hour != allHour {
		return "", "", nil, 0
	}
	if oneMin {
		return "hourly", "", nil, bits.TrailingZeros64(sp.min)
	}
	if step := minuteStep(sp.min); step > 0 {
		return "minutes", "", nil, step
	}
	return "", "", nil, 0
}

// minuteStep: die Minuten sind 0, n, 2n ... (n teilt 60), sonst 0.
func minuteStep(m uint64) int {
	c := bits.OnesCount64(m)
	if c < 2 || 60%c != 0 || m&1 == 0 {
		return 0
	}
	step := 60 / c
	for i := 0; i < 60; i += step {
		if m&(1<<uint(i)) == 0 {
			return 0
		}
	}
	return step
}

func two(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
