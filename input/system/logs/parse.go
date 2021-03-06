package logs

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pganalyze/collector/output/pganalyze_collector"
	"github.com/pganalyze/collector/state"
	uuid "github.com/satori/go.uuid"
)

const LogPrefixAmazonRds string = "%t:%r:%u@%d:[%p]:"
const LogPrefixCustom1 string = "%m [%p][%v] : [%l-1] %q[app=%a] "
const LogPrefixCustom2 string = "%t [%p-%l] %q%u@%d "

// Every one of these regexps should produce exactly one matching group
var TimeRegexp = `(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)? \w+)` // %t or %m
var IpAndPortRegexp = `([\d:.]+\(\d+\))?`                              // %r
var PidRegexp = `(\d+)`                                                // %p
var UserRegexp = `(\S*)`                                               // %u
var DbRegexp = `(\S*)`                                                 // %d
var AppRegexp = `(\S*)`                                                // %a
var VirtualTxRegexp = `(\d+/\d+)?`                                     // %v
var LogLineCounterRegexp = `(\d+)`                                     // %l
// Missing:
// - %h (host without port)
// - %n (unix timestamp)
// - %i (command tag)
// - %e (SQLSTATE)
// - %c (session ID)
// - %s (process start timestamp)
// - %x (transaction ID)

var LevelAndContentRegexp = `(\w+):\s+(.*\n?)$`
var LogPrefixAmazonRdsRegxp = regexp.MustCompile(`^` + TimeRegexp + `:` + IpAndPortRegexp + `:` + UserRegexp + `@` + DbRegexp + `:\[` + PidRegexp + `\]:` + LevelAndContentRegexp)
var LogPrefixCustom1Regexp = regexp.MustCompile(`^` + TimeRegexp + ` \[` + PidRegexp + `\]\[` + VirtualTxRegexp + `\] : \[` + LogLineCounterRegexp + `-1\] (?:\[app=` + AppRegexp + `\] )?` + LevelAndContentRegexp)
var LogPrefixCustom2Regexp = regexp.MustCompile(`^` + TimeRegexp + ` \[` + PidRegexp + `-` + LogLineCounterRegexp + `\] ` + `(?:` + UserRegexp + `@` + DbRegexp + ` )?` + LevelAndContentRegexp)
var LogPrefixNoTimestampUserDatabaseAppRegexp = regexp.MustCompile(`^\[user=` + UserRegexp + `,db=` + DbRegexp + `,app=` + AppRegexp + `\] ` + LevelAndContentRegexp)

var SyslogSequenceAndSplitRegexp = `(\[[\d-]+\])?`

var RsyslogLevelAndContentRegexp = `(?:(\w+):\s+)?(.*\n?)$`
var RsyslogTimeRegexp = `(\w+\s+\d+ \d{2}:\d{2}:\d{2})`
var RsyslogHostnameRegxp = `(\S+)`
var RsyslogProcessNameRegexp = `(\w+)`
var RsyslogRegexp = regexp.MustCompile(`^` + RsyslogTimeRegexp + ` ` + RsyslogHostnameRegxp + ` ` + RsyslogProcessNameRegexp + `\[` + PidRegexp + `\]: ` + SyslogSequenceAndSplitRegexp + ` ` + RsyslogLevelAndContentRegexp)

func ParseLogLineWithPrefix(prefix string, line string) (logLine state.LogLine, ok bool) {
	var timePart, userPart, dbPart, appPart, pidPart, levelPart, contentPart string

	// Assume Postgres time format unless overriden by the prefix (e.g. syslog)
	timeFormat := "2006-01-02 15:04:05 MST"

	rsyslog := false

	if prefix == "" {
		if LogPrefixAmazonRdsRegxp.MatchString(line) {
			prefix = LogPrefixAmazonRds
		} else if LogPrefixCustom1Regexp.MatchString(line) {
			prefix = LogPrefixCustom1
		} else if LogPrefixCustom2Regexp.MatchString(line) {
			prefix = LogPrefixCustom2
		} else if RsyslogRegexp.MatchString(line) {
			rsyslog = true
		}
	}

	if rsyslog {
		parts := RsyslogRegexp.FindStringSubmatch(line)
		if len(parts) == 0 {
			return
		}
		timeFormat = "2006 Jan  2 15:04:05"
		timePart = fmt.Sprintf("%d %s", time.Now().Year(), parts[1])
		// ignore syslog hostname
		// ignore syslog process name
		pidPart = parts[4]
		// ignore syslog postgres sequence and split number
		levelPart = parts[6]
		contentPart = strings.Replace(parts[7], "#011", "\t", -1)

		parts = LogPrefixNoTimestampUserDatabaseAppRegexp.FindStringSubmatch(contentPart)
		if len(parts) == 6 {
			userPart = parts[1]
			dbPart = parts[2]
			appPart = parts[3]
			levelPart = parts[4]
			contentPart = parts[5]
		}
	} else {
		switch prefix {
		case LogPrefixAmazonRds: // "%t:%r:%u@%d:[%p]:"
			parts := LogPrefixAmazonRdsRegxp.FindStringSubmatch(line)
			if len(parts) == 0 {
				return
			}

			timePart = parts[1]
			// skip %r (ip+port)
			userPart = parts[3]
			dbPart = parts[4]
			pidPart = parts[5]
			levelPart = parts[6]
			contentPart = parts[7]
		case LogPrefixCustom1: // "%m [%p][%v] : [%l-1] %q[app=%a] "
			parts := LogPrefixCustom1Regexp.FindStringSubmatch(line)
			if len(parts) == 0 {
				return
			}
			timePart = parts[1]
			pidPart = parts[2]
			// skip %v (virtual TX)
			// skip %l (log line counter)
			appPart = parts[5]
			levelPart = parts[6]
			contentPart = parts[7]
		case LogPrefixCustom2: // "%t [%p-1] %q%u@%d "
			parts := LogPrefixCustom2Regexp.FindStringSubmatch(line)
			if len(parts) == 0 {
				return
			}
			timePart = parts[1]
			pidPart = parts[2]
			// skip %l (log line counter)
			userPart = parts[4]
			dbPart = parts[5]
			levelPart = parts[6]
			contentPart = parts[7]
		default:
			// Some callers use the content of unparsed lines to stitch multi-line logs together
			logLine.Content = line
		}
	}

	var err error
	logLine.OccurredAt, err = time.Parse(timeFormat, timePart)
	if err != nil {
		ok = false
		return
	}

	if userPart != "[unknown]" {
		logLine.Username = userPart
	}
	if dbPart != "[unknown]" {
		logLine.Database = dbPart
	}
	if appPart != "[unknown]" {
		logLine.Application = appPart
	}

	backendPid, _ := strconv.Atoi(pidPart)
	logLine.BackendPid = int32(backendPid)
	logLine.Content = contentPart

	// This is actually a continuation of a previous line
	if levelPart == "" {
		return
	}

	logLine.LogLevel = pganalyze_collector.LogLineInformation_LogLevel(pganalyze_collector.LogLineInformation_LogLevel_value[levelPart])
	ok = true

	return
}

func ParseAndAnalyzeBuffer(buffer string, initialByteStart int64, linesNewerThan time.Time) ([]state.LogLine, []state.PostgresQuerySample, int64) {
	var logLines []state.LogLine
	currentByteStart := initialByteStart
	reader := bufio.NewReader(strings.NewReader(buffer))

	for {
		line, err := reader.ReadString('\n')
		byteStart := currentByteStart
		currentByteStart += int64(len(line))

		// This is intentionally after updating currentByteStart, since we consume the
		// data in the file even if an error is returned
		if err != nil {
			if err != io.EOF {
				fmt.Printf("Log Read ERROR: %s", err)
			}
			break
		}

		logLine, ok := ParseLogLineWithPrefix("", line)
		if !ok {
			// Assume that a parsing error in a follow-on line means that we actually
			// got additional data for the previous line
			if len(logLines) > 0 && logLine.Content != "" {
				logLines[len(logLines)-1].Content += logLine.Content
				logLines[len(logLines)-1].ByteEnd += int64(len(logLine.Content))
			}
			continue
		}

		// Ignore loglines which are outside our time window
		if logLine.OccurredAt.Before(linesNewerThan) {
			continue
		}

		logLine.ByteStart = byteStart
		logLine.ByteContentStart = byteStart + int64(len(line)-len(logLine.Content))
		logLine.ByteEnd = byteStart + int64(len(line)) - 1

		// Generate unique ID that can be used to reference this line
		logLine.UUID = uuid.NewV4()

		logLines = append(logLines, logLine)
	}

	newLogLines, newSamples := AnalyzeLogLines(logLines)
	return newLogLines, newSamples, currentByteStart
}
