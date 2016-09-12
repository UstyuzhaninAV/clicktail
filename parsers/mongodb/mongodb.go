// Package mongodb is a parser for mongodb logs
package mongodb

import (
	"errors"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/honeycombio/mongodbtools/logparser"

	"github.com/honeycombio/honeytail/event"
)

const (
	ctimeTimeFormat        = "Mon Jan _2 15:04:05.000"
	ctimeNoMSTimeFormat    = "Mon Jan _2 15:04:05"
	iso8601UTCTimeFormat   = "2006-01-02T15:04:05Z"
	iso8601LocalTimeFormat = "2006-01-02T15:04:05.999999999-0700"

	timestampFieldName  = "timestamp"
	namespaceFieldName  = "namespace"
	databaseFieldName   = "database"
	collectionFieldName = "collection"
	locksFieldName      = "locks"
)

var timestampFormats = []string{iso8601LocalTimeFormat, iso8601UTCTimeFormat, ctimeNoMSTimeFormat, ctimeTimeFormat}

type Options struct {
	LogPartials bool `long:"log_partials" description:"Send what was successfully parsed from a line (only if the error occured in the log line's message)."`
}

type Parser struct {
	conf       Options
	lineParser LineParser
	nower      Nower
}

type LineParser interface {
	ParseLogLine(line string) (map[string]interface{}, error)
}

type MongoLineParser struct {
}

func (m *MongoLineParser) ParseLogLine(line string) (map[string]interface{}, error) {
	return logparser.ParseLogLine(line)
}

func (p *Parser) Init(options interface{}) error {
	p.conf = *options.(*Options)
	p.nower = &RealNower{}
	p.lineParser = &MongoLineParser{}
	return nil
}

func (p *Parser) ProcessLines(lines <-chan string, send chan<- event.Event) {
	for line := range lines {
		values, err := p.lineParser.ParseLogLine(line)
		// we get a bunch of errors from the parser on mongo logs, skip em
		if err == nil || (p.conf.LogPartials && logparser.IsPartialLogLine(err)) {
			timestamp, err := p.parseTimestamp(values)
			if err != nil {
				logFailure(line, err, "couldn't parse logline timestamp, skipping")
				continue
			}
			if err = p.decomposeNamespace(values); err != nil {
				logFailure(line, err, "couldn't decompose logline namespace, skipping")
				continue
			}
			if err = p.decomposeLocks(values); err != nil {
				logFailure(line, err, "couldn't decompose logline locks, skipping")
				continue
			}

			logrus.WithFields(logrus.Fields{
				"line":   line,
				"values": values,
			}).Debug("Successfully parsed line")

			// we'll be putting the timestamp in the Event
			// itself, no need to also have it in the Data
			delete(values, timestampFieldName)

			send <- event.Event{
				Timestamp: timestamp,
				Data:      values,
			}
		} else {
			logFailure(line, err, "logline didn't parse, skipping.")
		}
	}
	logrus.Debug("lines channel is closed, ending mongo processor")
}

func (p *Parser) parseTimestamp(values map[string]interface{}) (time.Time, error) {
	now := p.nower.Now()
	timestamp_value, ok := values[timestampFieldName].(string)
	if ok {
		var err error
		for _, f := range timestampFormats {
			var timestamp time.Time
			timestamp, err = time.Parse(f, timestamp_value)
			if err == nil {
				if f == ctimeTimeFormat || f == ctimeNoMSTimeFormat {
					// these formats lacks the year, so we check
					// if adding Now().Year causes the date to be
					// after today.  if it's after today, we
					// decrement year by 1.  if it's not after, we
					// use it.
					ts := timestamp.AddDate(now.Year(), 0, 0)
					if now.After(ts) {
						return ts, nil
					}

					return timestamp.AddDate(now.Year()-1, 0, 0), nil
				}
				return timestamp, nil
			}
		}
		return time.Time{}, err
	}

	return time.Time{}, errors.New("timestamp missing from logline")
}

func (p *Parser) decomposeNamespace(values map[string]interface{}) error {
	ns_value, ok := values[namespaceFieldName]
	if !ok {
		return nil
	}

	decomposed := strings.SplitN(ns_value.(string), ".", 2)
	if len(decomposed) < 2 {
		return nil
	}
	values[databaseFieldName] = decomposed[0]
	values[collectionFieldName] = decomposed[1]
	return nil
}

func (p *Parser) decomposeLocks(values map[string]interface{}) error {
	locks_value, ok := values[locksFieldName]
	if !ok {
		return nil
	}
	locks_map, ok := locks_value.(map[string]interface{})
	if !ok {
		return nil
	}
	for scope, v := range locks_map {
		v_map, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		for attrKey, attrVal := range v_map {
			attrVal_map, ok := attrVal.(map[string]interface{})
			if !ok {
				continue
			}
			for lockType, lockCount := range attrVal_map {
				if lockType == "r" {
					lockType = "read"
				} else if lockType == "w" {
					lockType = "write"
				}

				if attrKey == "acquireCount" {
					values[strings.ToLower(scope)+"_"+lockType+"_lock"] = lockCount
				} else if attrKey == "acquireWaitCount" {
					values[strings.ToLower(scope)+"_"+lockType+"_lock_wait"] = lockCount
				}
			}
		}
	}
	return nil
}

func logFailure(line string, err error, msg string) {
	logrus.WithFields(logrus.Fields{"line": line}).WithError(err).Warnln(msg)
}

type Nower interface {
	Now() time.Time
}

type RealNower struct{}

func (r *RealNower) Now() time.Time {
	return time.Now().UTC()
}