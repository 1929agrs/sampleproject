package helpers

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/inconshreveable/log15.v2"
	"gopkg.in/natefinch/lumberjack.v2"
	"github.com/mattn/go-colorable"
	"path/filepath"
)

var VV string

func SetVV(vv string) { VV = vv }

func WritePid(file string) {
	if file != "" {
		_ = os.Remove(file)
		pid := os.Getpid()
		f, err := os.Create(file)
		if err != nil {
			log15.Crit("Can't create pidfile", "file", file, "err", err.Error())
			os.Exit(1)
		}
		_, err = f.WriteString(strconv.Itoa(pid) + "\n")
		if err != nil {
			log15.Crit("Can't write to pidfile", "file", file, "err", err.Error())
			os.Exit(1)
		}
		f.Close()
	}
}

// Create Logger Handlers
// 1. Stdout Handler - Terminal Format (Disabled)
// 2. Plain Log file Handler - Fmt Format
// 3. Json Log file Handler - Json Format (optional)
func createRotatingLoggerHandlers(terminalLog bool, plainLogFile, jsonLogFile string, lvl *log15.Lvl) log15.Handler {
	handlers := []log15.Handler{}
	maxFileSize := 100

	// Stdout Handler
	if terminalLog {
		handlers = append(handlers, log15.StreamHandler(colorable.NewColorableStdout(), log15.TerminalFormat()))
	}

	// Plain Log file Handler
	rotatingPlainLogger := &lumberjack.Logger{
		Filename:  plainLogFile,
		MaxSize:   maxFileSize,
		Compress:  true,
		LocalTime: true,
	}
	plainLogHdr := log15.StreamHandler(rotatingPlainLogger, log15.LogfmtFormat())
	if lvl != nil {
		filterLvl, _ := log15.LvlFromString(lvl.String())
		plainLogHdr = log15.LvlFilterHandler(filterLvl, plainLogHdr)
	}
	handlers = append(handlers, plainLogHdr)

	// Json Log file Handler
	if jsonLogFile != "" {
		rotatingJsonLogger := &lumberjack.Logger{
			Filename:  jsonLogFile,
			MaxSize:   maxFileSize,
			Compress:  true,
			LocalTime: true,
		}
		jsonLogHdr := log15.StreamHandler(rotatingJsonLogger, log15.JsonFormat())
		if lvl != nil {
			filterLvl, _ := log15.LvlFromString(lvl.String())
			jsonLogHdr = log15.LvlFilterHandler(filterLvl, jsonLogHdr)
		}
		handlers = append(handlers, jsonLogHdr)
	}

	return log15.MultiHandler(handlers...)
}

// Create Logger with Multiple Handlers
func CreateLogger(terminalLog bool, plainLogFile, jsonLogFile string) log15.Logger {

	multiHandler := createRotatingLoggerHandlers(terminalLog, plainLogFile, jsonLogFile, nil)

	logger := log15.New()
	logger.SetHandler(multiHandler)
	return logger
}

// Create Logger with Multiple Handlers
func CreateFilteredLogger(terminalLog bool, plainLogFile, jsonLogFile string, lvl log15.Lvl) log15.Logger {

	multiHandler := createRotatingLoggerHandlers(terminalLog, plainLogFile, jsonLogFile, &lvl)

	logger := log15.New()
	logger.SetHandler(multiHandler)
	return logger
}

func RegisterLogger(terminalLog bool, plainLogFile, jsonLogFile string) {
	multiHandler := createRotatingLoggerHandlers(terminalLog, plainLogFile, jsonLogFile, nil)
	log15.Root().SetHandler(multiHandler)
}

func RegisterFilteredLogger(terminalLog bool, plainLogFile, jsonLogFile string, lvl log15.Lvl) {
	multiHandler := createRotatingLoggerHandlers(terminalLog, plainLogFile, jsonLogFile, &lvl)
	log15.Root().SetHandler(multiHandler)
}

// Set logging to use the file or syslog, one of the them must be "" else an error ensues
func MakeLogger(pkg, file, facility string, maxLvl log15.Lvl) log15.Logger {
	var log log15.Logger
	if pkg != "" {
		log = log15.New("pkg", pkg)
	} else {
		log = log15.New()
	}
	if file != "" {
		if facility != "" {
			log.Crit("Can't log to syslog and logfile simultaneously")
			os.Exit(1)
		}
		log.Info("Switching logging", "file", file)
		h := log15.LvlFilterHandler(maxLvl, log15.Must.FileHandler(file, SimpleFormat(true)))
		/*if err != nil {
			log.Crit("Can't create log file", "file", file, "err", err.Error())
			os.Exit(1)
		}*/
		log15.Root().SetHandler(h)
		log.Info("Started logging here")
	} else if facility != "" {
		log.Info("Switching logging to syslog", "facility", facility)
		// TODO: Find cause for windows compilation error
		/*h, err := log15.SyslogHandler(facility, SimpleFormat(false))
		if err != nil {
			log.Crit("Can't connect to syslog", "err", err.Error())
			os.Exit(1)
		}*/
		h := log15.LvlFilterHandler(log15.LvlDebug, log15.StreamHandler(os.Stdout, SimpleFormat(false)))
		log15.Root().SetHandler(h)
		log.Info("Started logging here")
	} else {
		log.Info("WStunnel starting")
	}
	return log
}

const simpleTimeFormat = "2006-01-02 15:04:05"
const simpleMsgJust = 40

func SimpleFormat(timestamps bool) log15.Format {
	return log15.FormatFunc(func(r *log15.Record) []byte {
		b := &bytes.Buffer{}
		lvl := strings.ToUpper(r.Lvl.String())
		if timestamps {
			fmt.Fprintf(b, "[%s] %s %s ", r.Time.Format(simpleTimeFormat), lvl, r.Msg)
		} else {
			fmt.Fprintf(b, "%s %s ", lvl, r.Msg)
		}

		// try to justify the log output for short messages
		if len(r.Ctx) > 0 && len(r.Msg) < simpleMsgJust {
			b.Write(bytes.Repeat([]byte{' '}, simpleMsgJust-len(r.Msg)))
		}
		// print the keys logfmt style
		for i := 0; i < len(r.Ctx); i += 2 {
			if i != 0 {
				b.WriteByte(' ')
			}

			k, ok := r.Ctx[i].(string)
			v := formatLogfmtValue(r.Ctx[i+1])
			if !ok {
				k, v = "LOG_ERR", formatLogfmtValue(k)
			}

			// XXX: we should probably check that all of your key bytes aren't invalid
			fmt.Fprintf(b, "%s=%s", k, v)
		}

		b.WriteByte('\n')
		return b.Bytes()
	})
}

// copied from log15 https://github.com/inconshreveable/log15/blob/master/format.go#L203-L223
func formatLogfmtValue(value interface{}) string {
	if value == nil {
		return "nil"
	}

	value = formatShared(value)
	switch v := value.(type) {
	case bool:
		return strconv.FormatBool(v)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', 3, 64)
	case float64:
		return strconv.FormatFloat(v, 'f', 3, 64)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", value)
	case string:
		return escapeString(v)
	default:
		return escapeString(fmt.Sprintf("%+v", value))
	}
}

// copied from log15 https://github.com/inconshreveable/log15/blob/master/format.go
func formatShared(value interface{}) (result interface{}) {
	defer func() {
		if err := recover(); err != nil {
			if v := reflect.ValueOf(value); v.Kind() == reflect.Ptr && v.IsNil() {
				result = "nil"
			} else {
				panic(err)
			}
		}
	}()

	switch v := value.(type) {
	case time.Time:
		return v.Format(simpleTimeFormat)

	case error:
		return v.Error()

	case fmt.Stringer:
		return v.String()

	default:
		return v
	}
}

// copied from log15 https://github.com/inconshreveable/log15/blob/master/format.go
func escapeString(s string) string {
	needQuotes := false
	e := bytes.Buffer{}
	e.WriteByte('"')
	for _, r := range s {
		if r <= ' ' || r == '=' || r == '"' {
			needQuotes = true
		}

		switch r {
		case '\\', '"':
			e.WriteByte('\\')
			e.WriteByte(byte(r))
		case '\n':
			e.WriteByte('\\')
			e.WriteByte('n')
		case '\r':
			e.WriteByte('\\')
			e.WriteByte('r')
		case '\t':
			e.WriteByte('\\')
			e.WriteByte('t')
		default:
			e.WriteRune(r)
		}
	}
	e.WriteByte('"')
	start, stop := 0, e.Len()
	if !needQuotes {
		start, stop = 1, stop-1
	}
	return string(e.Bytes()[start:stop])
}

func CalcWsTimeout(tout int) time.Duration {
	var wsTimeout time.Duration
	if tout < 3 {
		wsTimeout = 3 * time.Second
	} else if tout > 600 {
		wsTimeout = 600 * time.Second
	} else {
		wsTimeout = time.Duration(tout) * time.Second
	}
	log15.Info("Setting WS keep-alive", "timeout", wsTimeout)
	return wsTimeout
}

// copy http headers over
func CopyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// Check if file exists
func Exists(name string) (bool, error) {
	_, err := os.Stat(name)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err != nil, err
}

func ExecutableFolder() string {
	ex, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return filepath.Dir(ex)
}
