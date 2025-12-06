package getparty

import (
	"fmt"
	"log"
)

const (
	DEBUG = iota
	INFO
	WARN
	ERRO
	lEVELS
)

func (m *Cmd) initLoggers() {
	m.loggers[DEBUG] = log.New(m.Err, fmt.Sprintf("[%s] ", cmdName), log.LstdFlags)
	m.loggers[INFO] = log.New(m.Out, "[INFO] ", log.LstdFlags)
	m.loggers[WARN] = log.New(m.Out, "[WARN] ", log.LstdFlags)
	m.loggers[ERRO] = log.New(m.Out, "[ERRO] ", log.LstdFlags)
}
