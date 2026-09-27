package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	barWidth             = 10
	opusInputPricePerM   = 15.0
	opusOutputPricePerM  = 75.0
)

var knownContextSizes = map[string]float64{
	"gemini": 1_000_000,
	"flash":  1_000_000,
	"pro":    2_000_000,
	"claude": 200_000,
	"sonnet": 200_000,
	"opus":   200_000,
	"haiku":  200_000,
	"gpt-4":  128_000,
	"o1":     200_000,
	"o3":     200_000,
}

var (
	ansiRegex     = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	effortRegex   = regexp.MustCompile(`(?i)^(.*?)\s*\((Low|Medium|High|None|Default|Max|[0-9]+k?)\)$`)
	geminiVFFirst = regexp.MustCompile(`(?i)^gemini[\s_-]+([0-9]+(?:\.[0-9]+)?)[\s_-]+(.*?)$`)
	geminiFFirst  = regexp.MustCompile(`(?i)^gemini[\s_-]+([a-zA-Z\s-]+?)[\s_-]+([0-9]+(?:\.[0-9]+)?.*?)$`)
	geminiRest    = regexp.MustCompile(`(?i)^gemini[\s_-]*(.*)$`)
)

func formatTokens(n float64) string {
	if n >= 1_000_000_000 {
		return fmt.Sprintf("%dB", int(n/1_000_000_000))
	} else if n >= 1_000_000 {
		return fmt.Sprintf("%dM", int(n/1_000_000))
	} else if n >= 1_000 {
		return fmt.Sprintf("%dk", int(n/1_000))
	}
	return fmt.Sprintf("%d", int(n))
}

func format1Dec(val float64) string {
	formatted := fmt.Sprintf("%.1f", val)
	if strings.HasSuffix(formatted, ".0") {
		return formatted[:len(formatted)-2]
	}
	return formatted
}

func formatContextSize(size float64) string {
	if size <= 0 {
		return ""
	}
	if size >= 1_000_000_000 {
		return format1Dec(size/1_000_000_000) + "G"
	} else if size >= 1_000_000 {
		return format1Dec(size/1_000_000) + "M"
	} else if size >= 1_000 {
		return format1Dec(size/1_000) + "K"
	}
	return fmt.Sprintf("%d", int(size))
}

func formatDuration(sec float64) string {
	seconds := int(sec)
	if seconds <= 0 {
		return "0m"
	}
	hours := seconds / 3600
	remainder := seconds % 3600
	minutes := remainder / 60
	if hours >= 24 {
		days := hours / 24
		hours = hours % 24
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	return fmt.Sprintf("%dh %02dm", hours, minutes)
}

func parseResetSeconds(bucket map[string]interface{}) (float64, bool) {
	if bucket == nil {
		return 0, false
	}
	if rtVal, ok := bucket["reset_time"].(string); ok && rtVal != "" {
		rtStr := strings.Replace(rtVal, "+00:00", "Z", 1)
		if t, err := time.Parse(time.RFC3339, rtStr); err == nil {
			delta := time.Until(t).Seconds()
			if delta >= 0 {
				return delta, true
			}
		}
	}
	if ris, ok := bucket["reset_in_seconds"]; ok && ris != nil {
		switch v := ris.(type) {
		case float64:
			return v, true
		case int:
			return float64(v), true
		case json.Number:
			if f, err := v.Float64(); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func findQuotaBucket(quotas map[string]interface{}, durationType, modelHint string) map[string]interface{} {
	if len(quotas) == 0 {
		return nil
	}

	modelHint = strings.ToLower(modelHint)
	var preferred []string
	if strings.Contains(modelHint, "gemini") || strings.Contains(modelHint, "flash") || strings.Contains(modelHint, "pro") ||
		strings.Contains(modelHint, "ultra") || strings.Contains(modelHint, "nano") || strings.Contains(modelHint, "gemma") {
		preferred = []string{"gemini", "3p"}
	} else if strings.Contains(modelHint, "claude") || strings.Contains(modelHint, "opus") || strings.Contains(modelHint, "sonnet") ||
		strings.Contains(modelHint, "haiku") || strings.Contains(modelHint, "anthropic") || strings.Contains(modelHint, "gpt") ||
		strings.Contains(modelHint, "openai") || strings.Contains(modelHint, "3p") {
		preferred = []string{"3p", "claude", "anthropic", "gemini"}
	} else {
		preferred = []string{"gemini", "3p"}
	}

	durationPatterns := []string{durationType}
	if durationType == "5h" {
		durationPatterns = []string{"5h", "5_hour", "5-hour", "five"}
	} else if durationType == "weekly" {
		durationPatterns = []string{"week", "7d", "7_day", "seven"}
	}

	hasPattern := func(key string) bool {
		for _, dp := range durationPatterns {
			if strings.Contains(key, dp) {
				return true
			}
		}
		return false
	}

	// 1. Match preferred model prefix and duration
	for _, pref := range preferred {
		for k, v := range quotas {
			kLower := strings.ToLower(k)
			if strings.Contains(kLower, pref) && hasPattern(kLower) {
				if b, ok := v.(map[string]interface{}); ok {
					return b
				}
			}
		}
	}

	// 2. Match any bucket with duration that is actively consumed (< 1.0)
	for k, v := range quotas {
		kLower := strings.ToLower(k)
		if hasPattern(kLower) {
			if b, ok := v.(map[string]interface{}); ok {
				rem := getFloat(b, "remaining_fraction", 1.0)
				if rem < 1.0 {
					return b
				}
			}
		}
	}

	// 3. Fallback to any bucket matching duration
	for k, v := range quotas {
		kLower := strings.ToLower(k)
		if hasPattern(kLower) {
			if b, ok := v.(map[string]interface{}); ok {
				return b
			}
		}
	}

	return nil
}

func getFloat(m map[string]interface{}, key string, def float64) float64 {
	if v, ok := m[key]; ok && v != nil {
		switch num := v.(type) {
		case float64:
			return num
		case int:
			return float64(num)
		case json.Number:
			if f, err := num.Float64(); err == nil {
				return f
			}
		}
	}
	return def
}

func getGradientRGB(ratio float64, reverse bool) (int, int, int) {
	val := math.Max(0.0, math.Min(1.0, ratio))
	if reverse {
		val = 1.0 - val
	}
	if val <= 0.5 {
		t := val * 2.0
		r := int(30.0 + (255.0-30.0)*t)
		g := 215
		b := int(96.0 + (0.0-96.0)*t)
		return r, g, b
	}
	t := (val - 0.5) * 2.0
	r := 255
	g := int(215.0 + (60.0-215.0)*t)
	b := int(0.0 + (60.0-0.0)*t)
	return r, g, b
}

func renderBar(remFrac float64, width int) string {
	remFrac = math.Max(0.0, math.Min(1.0, remFrac))
	filled := int(remFrac*float64(width) + 0.5)
	empty := width - filled
	r, g, b := getGradientRGB(remFrac, true)
	color := fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
	fullBlocks := strings.Repeat("█", filled)
	emptyBlocks := strings.Repeat("░", empty)
	if filled == 0 {
		return fmt.Sprintf("\033[38;2;255;60;60m%s\033[0m", strings.Repeat("░", width))
	}
	return fmt.Sprintf("%s%s\033[90m%s\033[0m", color, fullBlocks, emptyBlocks)
}

func formatQuotaDisplay(bucket map[string]interface{}) string {
	if bucket == nil {
		return ""
	}

	var remFrac float64 = 1.0
	if rf, ok := bucket["remaining_fraction"]; ok && rf != nil {
		remFrac = getFloat(bucket, "remaining_fraction", 1.0)
	} else if rp, ok := bucket["remaining_percentage"]; ok && rp != nil {
		val := getFloat(bucket, "remaining_percentage", 100.0)
		if val > 1.0 {
			remFrac = val / 100.0
		} else {
			remFrac = val
		}
	} else if up, ok := bucket["used_percentage"]; ok && up != nil {
		val := getFloat(bucket, "used_percentage", 0.0)
		usedFrac := val
		if val > 1.0 {
			usedFrac = val / 100.0
		}
		remFrac = 1.0 - usedFrac
	} else if uf, ok := bucket["used_fraction"]; ok && uf != nil {
		remFrac = 1.0 - getFloat(bucket, "used_fraction", 0.0)
	}

	remFrac = math.Max(0.0, math.Min(1.0, remFrac))
	pctVal := remFrac * 100.0

	var pctStr string
	if pctVal >= 100.0 {
		pctStr = "100%"
	} else {
		pctStr = fmt.Sprintf("%d%%", int(pctVal))
	}

	barStr := renderBar(remFrac, barWidth)
	sec, hasReset := parseResetSeconds(bucket)
	var timeStr string
	if hasReset {
		timeStr = formatDuration(sec)
	}

	parts := []string{pctStr, barStr}
	if timeStr != "" {
		parts = append(parts, timeStr)
	}
	return strings.Join(parts, " ")
}

func titleCaseIfLower(s string) string {
	isAllLower := true
	for _, r := range s {
		if unicode.IsLetter(r) && !unicode.IsLower(r) {
			isAllLower = false
			break
		}
	}
	if isAllLower && len(s) > 0 {
		return strings.Title(s)
	}
	return s
}

func formatModelName(name string) string {
	if name == "" || !strings.Contains(strings.ToLower(name), "gemini") {
		return name
	}

	// 1. "Gemini <version> <family>" -> e.g. "Gemini 3.8 Flash" -> "Flash 3.8"
	if m := geminiVFFirst.FindStringSubmatch(name); m != nil {
		version := strings.TrimSpace(m[1])
		family := titleCaseIfLower(strings.TrimSpace(m[2]))
		return fmt.Sprintf("%s %s", family, version)
	}

	// 2. "Gemini <family> <version>" -> e.g. "Gemini Flash 3.8" -> "Flash 3.8"
	if m := geminiFFirst.FindStringSubmatch(name); m != nil {
		family := titleCaseIfLower(strings.TrimSpace(m[1]))
		version := strings.TrimSpace(m[2])
		return fmt.Sprintf("%s %s", family, version)
	}

	// 3. "Gemini <anything else>" -> strip "Gemini "
	if m := geminiRest.FindStringSubmatch(name); m != nil && strings.TrimSpace(m[1]) != "" {
		rest := strings.TrimSpace(m[1])
		return titleCaseIfLower(rest)
	}

	return name
}

func getModelAndEffort(data map[string]interface{}) string {
	var modelName string
	var effort string

	if modelObj, ok := data["model"].(map[string]interface{}); ok {
		if dn, ok := modelObj["display_name"].(string); ok && dn != "" {
			modelName = dn
		} else if n, ok := modelObj["name"].(string); ok && n != "" {
			modelName = n
		} else if id, ok := modelObj["id"].(string); ok && id != "" {
			modelName = id
		}
		if te, ok := modelObj["thinking_effort"].(string); ok && te != "" {
			effort = te
		} else if ef, ok := modelObj["effort"].(string); ok && ef != "" {
			effort = ef
		} else if re, ok := modelObj["reasoning_effort"].(string); ok && re != "" {
			effort = re
		}
	} else if ms, ok := data["model"].(string); ok && strings.TrimSpace(ms) != "" {
		modelName = strings.TrimSpace(ms)
	}

	if effort == "" {
		for _, k := range []string{"thinking", "reasoning"} {
			if tObj, ok := data[k].(map[string]interface{}); ok {
				if ef, ok := tObj["effort"].(string); ok && ef != "" {
					effort = ef
					break
				} else if lvl, ok := tObj["level"].(string); ok && lvl != "" {
					effort = lvl
					break
				}
			} else if ts, ok := data[k].(string); ok && strings.TrimSpace(ts) != "" {
				effort = strings.TrimSpace(ts)
				break
			}
		}
	}
	if effort == "" {
		for _, k := range []string{"thinking_effort", "reasoning_effort", "effort"} {
			if s, ok := data[k].(string); ok && s != "" {
				effort = s
				break
			}
		}
	}

	if modelName == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			settingsPath := filepath.Join(home, ".gemini/antigravity-cli/settings.json")
			if b, err := os.ReadFile(settingsPath); err == nil {
				var settings map[string]interface{}
				if json.Unmarshal(b, &settings) == nil {
					if m, ok := settings["model"].(string); ok && m != "" {
						modelName = m
					}
				}
			}
		}
	}

	if modelName == "" {
		modelName = "Gemini"
	}

	if m := effortRegex.FindStringSubmatch(modelName); m != nil {
		modelName = strings.TrimSpace(m[1])
		if effort == "" {
			effort = strings.TrimSpace(m[2])
		}
	}

	modelName = formatModelName(modelName)

	if effort != "" {
		return fmt.Sprintf("%s • %s", modelName, effort)
	}
	return modelName
}

func visibleLen(s string) int {
	clean := ansiRegex.ReplaceAllString(s, "")
	return utf8.RuneCountInString(clean)
}

func getTerminalColumns(data map[string]interface{}) int {
	if tw, ok := data["terminal_width"]; ok && tw != nil {
		switch v := tw.(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			if i, err := v.Int64(); err == nil {
				return int(i)
			}
		}
	}
	if colsStr := os.Getenv("COLUMNS"); colsStr != "" {
		if c, err := strconv.Atoi(colsStr); err == nil && c > 0 {
			return c
		}
	}
	return 100
}

func main() {
	rawData, err := io.ReadAll(os.Stdin)
	if err != nil || len(strings.TrimSpace(string(rawData))) == 0 {
		return
	}

	// Persist last payload for debugging
	home, _ := os.UserHomeDir()
	if home != "" {
		payloadPath := filepath.Join(home, ".gemini/antigravity-cli/statusline_last_payload.json")
		_ = os.WriteFile(payloadPath, rawData, 0644)
	}

	var data map[string]interface{}
	d := json.NewDecoder(strings.NewReader(string(rawData)))
	d.UseNumber()
	if err := d.Decode(&data); err != nil {
		return
	}

	ctx, _ := data["context_window"].(map[string]interface{})
	if ctx == nil {
		ctx = make(map[string]interface{})
	}

	// 1. Cost Estimate based on Anthropic Opus rates
	inTok := getFloat(ctx, "total_input_tokens", getFloat(data, "total_input_tokens", 0.0))
	outTok := getFloat(ctx, "total_output_tokens", getFloat(data, "total_output_tokens", 0.0))
	cost := (inTok*opusInputPricePerM + outTok*opusOutputPricePerM) / 1_000_000.0

	costRatio := math.Min(1.0, math.Max(0.0, cost/10.0))
	rCost, gCost, bCost := getGradientRGB(costRatio, false)
	costValStr := fmt.Sprintf("\033[1;38;2;%d;%d;%dm$%.2f\033[0m", rCost, gCost, bCost, cost)
	costStr := fmt.Sprintf("%s [↑%s ↓%s]", costValStr, formatTokens(inTok), formatTokens(outTok))

	// 2. Context Window Usage + Size
	modelStr := getModelAndEffort(data)
	size := getFloat(ctx, "context_window_size", 0.0)
	if size <= 0 {
		modelLower := strings.ToLower(modelStr)
		for k, v := range knownContextSizes {
			if strings.Contains(modelLower, k) {
				size = v
				break
			}
		}
	}

	sizeLabel := formatContextSize(size)
	usedPct := -1.0
	if v, ok := ctx["used_percentage"]; ok && v != nil {
		usedPct = getFloat(ctx, "used_percentage", -1.0)
	} else if size > 0 {
		totalTok := inTok + outTok
		usedPct = (totalTok / size) * 100.0
	}

	var ctxPctStr string
	if usedPct >= 0 {
		pctVal := usedPct
		if usedPct <= 1.0 {
			pctVal = usedPct * 100.0
		}
		pctRatio := pctVal / 100.0
		r, g, b := getGradientRGB(pctRatio, false)
		pctText := fmt.Sprintf("%s%%", format1Dec(pctVal))
		ctxPctStr = fmt.Sprintf("\033[1;38;2;%d;%d;%dm%s\033[0m", r, g, b, pctText)
	} else {
		r, g, b := getGradientRGB(0.0, false)
		ctxPctStr = fmt.Sprintf("\033[1;38;2;%d;%d;%dm0%%\033[0m", r, g, b)
	}

	var ctxStr string
	if sizeLabel != "" {
		ctxStr = fmt.Sprintf("%s [%s]", ctxPctStr, sizeLabel)
	} else {
		ctxStr = ctxPctStr
	}

	// 3. Quotas
	var quotas map[string]interface{}
	if q, ok := data["quota"].(map[string]interface{}); ok {
		quotas = q
	} else if q, ok := data["quotas"].(map[string]interface{}); ok {
		quotas = q
	}

	bucket5h := findQuotaBucket(quotas, "5h", modelStr)
	bucketWeek := findQuotaBucket(quotas, "weekly", modelStr)

	info5h := formatQuotaDisplay(bucket5h)
	infoWeek := formatQuotaDisplay(bucketWeek)

	leftParts := []string{costStr, ctxStr}
	if info5h != "" {
		leftParts = append(leftParts, info5h)
	}
	if infoWeek != "" {
		leftParts = append(leftParts, infoWeek)
	}

	leftStr := strings.Join(leftParts, " │ ")
	rightStr := modelStr

	cols := getTerminalColumns(data)
	padding := cols - visibleLen(leftStr) - visibleLen(rightStr) - 1

	var line string
	if padding > 1 {
		line = leftStr + strings.Repeat(" ", padding) + rightStr
	} else {
		line = leftStr + " │ " + rightStr
	}

	fmt.Println(line)
}
