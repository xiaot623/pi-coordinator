package crons

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Schedule struct {
	minute field
	hour   field
	dom    field
	month  field
	dow    field
}

type field struct {
	min      int
	max      int
	wildcard bool
	values   map[int]bool
}

func Parse(raw string) (Schedule, error) {
	parts := strings.Fields(strings.TrimSpace(raw))
	if len(parts) != 5 {
		return Schedule{}, fmt.Errorf("cron schedule must have 5 fields")
	}
	minute, err := parseField(parts[0], 0, 59, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("minute: %w", err)
	}
	hour, err := parseField(parts[1], 0, 23, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("hour: %w", err)
	}
	dom, err := parseField(parts[2], 1, 31, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("day-of-month: %w", err)
	}
	month, err := parseField(parts[3], 1, 12, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("month: %w", err)
	}
	dow, err := parseField(parts[4], 0, 7, true)
	if err != nil {
		return Schedule{}, fmt.Errorf("day-of-week: %w", err)
	}
	return Schedule{minute: minute, hour: hour, dom: dom, month: month, dow: dow}, nil
}

func MustParse(raw string) Schedule {
	s, err := Parse(raw)
	if err != nil {
		panic(err)
	}
	return s
}

func Matches(raw string, t time.Time) (bool, error) {
	s, err := Parse(raw)
	if err != nil {
		return false, err
	}
	return s.Match(t), nil
}

func Next(raw string, after time.Time) (time.Time, error) {
	s, err := Parse(raw)
	if err != nil {
		return time.Time{}, err
	}
	return s.Next(after), nil
}

func (s Schedule) Match(t time.Time) bool {
	if !s.minute.contains(t.Minute()) || !s.hour.contains(t.Hour()) || !s.month.contains(int(t.Month())) {
		return false
	}
	domMatch := s.dom.contains(t.Day())
	dowMatch := s.dow.contains(int(t.Weekday()))
	if !s.dom.wildcard && !s.dow.wildcard {
		return domMatch || dowMatch
	}
	return domMatch && dowMatch
}

func (s Schedule) Next(after time.Time) time.Time {
	candidate := after.Truncate(time.Minute).Add(time.Minute)
	limit := candidate.AddDate(5, 0, 0)
	for !candidate.After(limit) {
		if s.Match(candidate) {
			return candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}
}

func parseField(raw string, minValue, maxValue int, allowSevenSunday bool) (field, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return field{}, fmt.Errorf("empty field")
	}
	out := field{min: minValue, max: maxValue, values: make(map[int]bool)}
	if raw == "*" {
		out.wildcard = true
	}
	parts := strings.Split(raw, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return field{}, fmt.Errorf("empty list item")
		}
		if err := addFieldPart(&out, part, minValue, maxValue, allowSevenSunday); err != nil {
			return field{}, err
		}
	}
	return out, nil
}

func addFieldPart(out *field, raw string, minValue, maxValue int, allowSevenSunday bool) error {
	base := raw
	step := 1
	if strings.Contains(raw, "/") {
		pieces := strings.Split(raw, "/")
		if len(pieces) != 2 || pieces[0] == "" || pieces[1] == "" {
			return fmt.Errorf("invalid step %q", raw)
		}
		base = pieces[0]
		parsedStep, err := strconv.Atoi(pieces[1])
		if err != nil || parsedStep <= 0 {
			return fmt.Errorf("invalid step %q", raw)
		}
		step = parsedStep
	}
	start, end := minValue, maxValue
	switch {
	case base == "*":
		out.wildcard = step == 1
	case strings.Contains(base, "-"):
		pieces := strings.Split(base, "-")
		if len(pieces) != 2 {
			return fmt.Errorf("invalid range %q", raw)
		}
		var err error
		start, err = parseFieldNumber(pieces[0], minValue, maxValue, allowSevenSunday)
		if err != nil {
			return err
		}
		end, err = parseFieldNumber(pieces[1], minValue, maxValue, allowSevenSunday)
		if err != nil {
			return err
		}
		if start > end {
			return fmt.Errorf("range start greater than end in %q", raw)
		}
	default:
		value, err := parseFieldNumber(base, minValue, maxValue, allowSevenSunday)
		if err != nil {
			return err
		}
		if allowSevenSunday && value == 7 {
			value = 0
		}
		start, end = value, value
	}
	for v := start; v <= end; v += step {
		out.values[v] = true
	}
	return nil
}

func parseFieldNumber(raw string, minValue, maxValue int, allowSevenSunday bool) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", raw)
	}
	if allowSevenSunday && value == 7 {
		return value, nil
	}
	if value < minValue || value > maxValue {
		return 0, fmt.Errorf("value %d outside %d-%d", value, minValue, maxValue)
	}
	return value, nil
}

func (f field) contains(value int) bool {
	if f.values[value] {
		return true
	}
	if f.max == 7 && value == 0 && f.values[7] {
		return true
	}
	return false
}
