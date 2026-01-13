package getparty

import (
	"fmt"
	"io"
	"log"
)

const (
	DBUG = iota
	INFO
	WARN
	ERRO
	lEVELS
)

func (m *Cmd) initLoggers() error {
	if m.opt == nil {
		return ErrBadInvariant
	}
	if m.opt.Quiet {
		m.Out = io.Discard
	}
	if !m.opt.Debug {
		m.Err = io.Discard
	}

	m.loggers[DBUG] = log.New(m.Err, fmt.Sprintf("[%s] ", cmdName), log.LstdFlags)
	m.loggers[INFO] = log.New(m.Out, "[INFO] ", log.LstdFlags)
	m.loggers[WARN] = log.New(m.Out, "[WARN] ", log.LstdFlags)
	m.loggers[ERRO] = log.New(m.Out, "[ERRO] ", log.LstdFlags)

	return nil
}
