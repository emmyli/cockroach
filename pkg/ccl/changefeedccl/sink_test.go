// Copyright 2018 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package changefeedccl

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Shopify/sarama"
	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/pkg/errors"
)

type asyncProducerMock struct {
	inputCh     chan *sarama.ProducerMessage
	successesCh chan *sarama.ProducerMessage
	errorsCh    chan *sarama.ProducerError
}

func (p asyncProducerMock) Input() chan<- *sarama.ProducerMessage     { return p.inputCh }
func (p asyncProducerMock) Successes() <-chan *sarama.ProducerMessage { return p.successesCh }
func (p asyncProducerMock) Errors() <-chan *sarama.ProducerError      { return p.errorsCh }
func (p asyncProducerMock) AsyncClose()                               { panic(`unimplemented`) }
func (p asyncProducerMock) Close() error {
	close(p.inputCh)
	close(p.successesCh)
	close(p.errorsCh)
	return nil
}

func TestKafkaSink(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	p := asyncProducerMock{
		inputCh:     make(chan *sarama.ProducerMessage, 1),
		successesCh: make(chan *sarama.ProducerMessage, 1),
		errorsCh:    make(chan *sarama.ProducerError, 1),
	}
	sink := &kafkaSink{
		producer: p,
		topics:   map[string]struct{}{`t`: {}},
	}
	sink.start()
	defer func() {
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// No inflight
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// Timeout
	if err := sink.EmitRow(ctx, `t`, []byte(`1`), nil); err != nil {
		t.Fatal(err)
	}
	m1 := <-p.inputCh
	for i := 0; i < 2; i++ {
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		if err := sink.Flush(timeoutCtx); !testutils.IsError(err, `context deadline exceeded`) {
			t.Fatalf(`expected "context deadline exceeded" error got: %+v`, err)
		}
	}
	go func() { p.successesCh <- m1 }()
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// Check no inflight again now that we've sent something
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// Mixed success and error.
	if err := sink.EmitRow(ctx, `t`, []byte(`2`), nil); err != nil {
		t.Fatal(err)
	}
	m2 := <-p.inputCh
	if err := sink.EmitRow(ctx, `t`, []byte(`3`), nil); err != nil {
		t.Fatal(err)
	}
	m3 := <-p.inputCh
	if err := sink.EmitRow(ctx, `t`, []byte(`4`), nil); err != nil {
		t.Fatal(err)
	}
	m4 := <-p.inputCh
	go func() { p.successesCh <- m2 }()
	go func() {
		p.errorsCh <- &sarama.ProducerError{
			Msg: m3,
			Err: errors.New("m3"),
		}
	}()
	go func() { p.successesCh <- m4 }()
	if err := sink.Flush(ctx); !testutils.IsError(err, `m3`) {
		t.Fatalf(`expected "m3" error got: %+v`, err)
	}

	// Check simple success again after error
	if err := sink.EmitRow(ctx, `t`, []byte(`5`), nil); err != nil {
		t.Fatal(err)
	}
	m5 := <-p.inputCh
	go func() { p.successesCh <- m5 }()
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSink(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	s, sqlDBRaw, _ := serverutils.StartServer(t, base.TestServerArgs{UseDatabase: "d"})
	defer s.Stopper().Stop(ctx)
	sqlDB := sqlutils.MakeSQLRunner(sqlDBRaw)
	sqlDB.Exec(t, `CREATE DATABASE d`)

	sinkURL, cleanup := sqlutils.PGUrl(t, s.ServingAddr(), t.Name(), url.User(security.RootUser))
	defer cleanup()
	sinkURL.Path = `d`

	targets := jobspb.ChangefeedTargets{
		0: jobspb.ChangefeedTarget{StatementTimeName: `foo`},
		1: jobspb.ChangefeedTarget{StatementTimeName: `bar`},
	}
	sink, err := makeSQLSink(sinkURL.String(), `sink`, targets)
	require.NoError(t, err)
	defer func() { require.NoError(t, sink.Close()) }()

	// Empty
	require.NoError(t, sink.Flush(ctx))

	// Undeclared topic
	require.EqualError(t, sink.EmitRow(ctx, `nope`, nil, nil), `cannot emit to undeclared topic: nope`)

	// With one row, nothing flushes until Flush is called.
	require.NoError(t, sink.EmitRow(ctx, `foo`, []byte(`k1`), []byte(`v0`)))
	sqlDB.CheckQueryResults(t, `SELECT key, value FROM sink ORDER BY PRIMARY KEY sink`,
		[][]string{},
	)
	require.NoError(t, sink.Flush(ctx))
	sqlDB.CheckQueryResults(t, `SELECT key, value FROM sink ORDER BY PRIMARY KEY sink`,
		[][]string{{`k1`, `v0`}},
	)
	sqlDB.Exec(t, `TRUNCATE sink`)

	// Verify the implicit flushing
	sqlDB.CheckQueryResults(t, `SELECT count(*) FROM sink`, [][]string{{`0`}})
	for i := 0; i < sqlSinkRowBatchSize+1; i++ {
		require.NoError(t, sink.EmitRow(ctx, `foo`, []byte(`k1`), []byte(`v`+strconv.Itoa(i))))
	}
	// Should have auto flushed after sqlSinkRowBatchSize
	sqlDB.CheckQueryResults(t, `SELECT count(*) FROM sink`, [][]string{{`3`}})
	require.NoError(t, sink.Flush(ctx))
	sqlDB.CheckQueryResults(t, `SELECT count(*) FROM sink`, [][]string{{`4`}})
	sqlDB.Exec(t, `TRUNCATE sink`)

	// Two tables interleaved in time
	require.NoError(t, sink.EmitRow(ctx, `foo`, []byte(`kfoo`), []byte(`v0`)))
	require.NoError(t, sink.EmitRow(ctx, `bar`, []byte(`kbar`), []byte(`v0`)))
	require.NoError(t, sink.EmitRow(ctx, `foo`, []byte(`kfoo`), []byte(`v1`)))
	require.NoError(t, sink.Flush(ctx))
	sqlDB.CheckQueryResults(t, `SELECT topic, key, value FROM sink ORDER BY PRIMARY KEY sink`,
		[][]string{{`bar`, `kbar`, `v0`}, {`foo`, `kfoo`, `v0`}, {`foo`, `kfoo`, `v1`}},
	)
	sqlDB.Exec(t, `TRUNCATE sink`)

	// Multiple keys interleaved in time. Use sqlSinkNumPartitions+1 keys to
	// guarantee that at lease two of them end up in the same partition.
	for i := 0; i < sqlSinkNumPartitions+1; i++ {
		require.NoError(t, sink.EmitRow(ctx, `foo`, []byte(`v`+strconv.Itoa(i)), []byte(`v0`)))
	}
	for i := 0; i < sqlSinkNumPartitions+1; i++ {
		require.NoError(t, sink.EmitRow(ctx, `foo`, []byte(`v`+strconv.Itoa(i)), []byte(`v1`)))
	}
	require.NoError(t, sink.Flush(ctx))
	sqlDB.CheckQueryResults(t, `SELECT partition, key, value FROM sink ORDER BY PRIMARY KEY sink`,
		[][]string{
			{`0`, `v3`, `v0`},
			{`0`, `v3`, `v1`},
			{`1`, `v1`, `v0`},
			{`1`, `v2`, `v0`},
			{`1`, `v1`, `v1`},
			{`1`, `v2`, `v1`},
			{`2`, `v0`, `v0`},
			{`2`, `v0`, `v1`},
		},
	)
	sqlDB.Exec(t, `TRUNCATE sink`)

	// Emit resolved
	require.NoError(t, sink.EmitResolvedTimestamp(ctx, []byte(`r0`)))
	require.NoError(t, sink.EmitRow(ctx, `foo`, []byte(`foo0`), []byte(`v0`)))
	require.NoError(t, sink.EmitResolvedTimestamp(ctx, []byte(`r1`)))
	require.NoError(t, sink.Flush(ctx))
	sqlDB.CheckQueryResults(t,
		`SELECT topic, partition, key, value, resolved FROM sink ORDER BY PRIMARY KEY sink`,
		[][]string{
			{`bar`, `0`, ``, ``, `r0`},
			{`bar`, `0`, ``, ``, `r1`},
			{`bar`, `1`, ``, ``, `r0`},
			{`bar`, `1`, ``, ``, `r1`},
			{`bar`, `2`, ``, ``, `r0`},
			{`bar`, `2`, ``, ``, `r1`},
			{`foo`, `0`, ``, ``, `r0`},
			{`foo`, `0`, `foo0`, `v0`, ``},
			{`foo`, `0`, ``, ``, `r1`},
			{`foo`, `1`, ``, ``, `r0`},
			{`foo`, `1`, ``, ``, `r1`},
			{`foo`, `2`, ``, ``, `r0`},
			{`foo`, `2`, ``, ``, `r1`},
		},
	)
}
