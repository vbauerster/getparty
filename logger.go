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
	out := m.getOut()
	m.loggers[DEBUG] = log.New(m.getErr(), fmt.Sprintf("[%s] ", cmdName), log.LstdFlags)
	m.loggers[INFO] = log.New(out, "[INFO] ", log.LstdFlags)
	m.loggers[WARN] = log.New(out, "[WARN] ", log.LstdFlags)
	m.loggers[ERRO] = log.New(out, "[ERRO] ", log.LstdFlags)
}
