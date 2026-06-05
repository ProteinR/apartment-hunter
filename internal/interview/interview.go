package interview

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/geocoding"
)

type Interview struct {
	in       *bufio.Reader
	out      io.Writer
	resolver geocoding.Resolver
}

func New(in io.Reader, out io.Writer) Interview {
	return NewWithResolver(in, out, nil)
}

func NewWithResolver(in io.Reader, out io.Writer, resolver geocoding.Resolver) Interview {
	return Interview{in: bufio.NewReader(in), out: out, resolver: resolver}
}

func (i Interview) Run() (domain.UserPreferences, error) {
	session := NewSessionWithResolver(i.resolver)
	for {
		question, ok := session.CurrentQuestion()
		if !ok {
			return session.Preferences(), nil
		}

		for {
			answer := i.askString(question.Prompt)
			_, done, err := session.Answer(answer)
			if err != nil {
				fmt.Fprintln(i.out, err)
				continue
			}
			if done {
				return session.Preferences(), nil
			}
			break
		}
	}
}

func (i Interview) askString(prompt string) string {
	fmt.Fprintf(i.out, "%s: ", prompt)
	line, _ := i.in.ReadString('\n')
	return strings.TrimSpace(line)
}
