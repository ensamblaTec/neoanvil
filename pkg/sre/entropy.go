package sre

import (
	"crypto/rand"
	"time"
)

// JitterEntropy Estocástico. Libre de las ataduras humanas y solares.
type JitterEntropy struct{}

func (j *JitterEntropy) Read(p []byte) (n int, err error) {
	n, err = rand.Read(p)

	for i := range p {
		// SRE Rule: Nunca usar .UnixNano() porque depende del reloj de pared ajustado por NTP
		// En su lugar, el objeto time.Time() incorpora una lectura Monotónica oculta de hardware (uptime)
		t1 := time.Now()
		t2 := time.Now()
		// .Sub() obligatoriamente extrae y contrasta los temporizadores monotónicos ciegos!
		p[i] ^= byte(t2.Sub(t1).Nanoseconds())
	}
	return n, err
}
