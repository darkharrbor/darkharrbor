package nntp

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
)

func TestRecoverCrossLaneSpanVerifiesIFSCBlocks(t *testing.T) {
	req, reader, media, segments := fixtureRequest(t, 1)
	recover := func(ctx context.Context, _ string, _ int, size int64, proof PAR2Proof) (CrossLaneBlock, error) {
		if size != int64(len(media)) {
			t.Fatalf("file size=%d want=%d", size, len(media))
		}
		end := proof.Offset + proof.Length
		return CrossLaneBlock{
			Data: append([]byte(nil), media[proof.Offset:end]...),
			Lane: contentproof.LaneHTTP,
		}, nil
	}
	got, err := recoverCrossLaneSpan(context.Background(), reader, "item", 0, 1, int64(len(media)), req.Proof, req.Budget, req.Now, recover)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.data, segments[1]) {
		t.Fatalf("recovered segment differs: got=%d want=%d", len(got.data), len(segments[1]))
	}
	if got.blocks <= 0 || got.httpBlocks != int(got.blocks) || got.torrentBlocks != 0 {
		t.Fatalf("unexpected counters: %+v", got)
	}
}

func TestRecoverCrossLaneSpanFailsClosed(t *testing.T) {
	req, _, media, _ := fixtureRequest(t, 1)
	valid := func(_ context.Context, _ string, _ int, _ int64, proof PAR2Proof) (CrossLaneBlock, error) {
		end := proof.Offset + proof.Length
		return CrossLaneBlock{Data: append([]byte(nil), media[proof.Offset:end]...), Lane: contentproof.LaneTorrent}, nil
	}
	cases := []struct {
		name    string
		mutate  func(*fixtureReader, *PAR2ProofDomain, *PAR2RepairBudget, *CrossLaneRecover)
		wantErr error
	}{
		{
			name: "no proof",
			mutate: func(_ *fixtureReader, proof *PAR2ProofDomain, _ *PAR2RepairBudget, _ *CrossLaneRecover) {
				*proof = PAR2ProofDomain{}
			},
			wantErr: ErrCrossLaneAbstain,
		},
		{
			name: "second missing segment",
			mutate: func(reader *fixtureReader, _ *PAR2ProofDomain, _ *PAR2RepairBudget, _ *CrossLaneRecover) {
				reader.failRead[[2]int{0, 2}] = errors.New("missing")
			},
			wantErr: ErrPAR2RepairAbstain,
		},
		{
			name: "byte budget",
			mutate: func(_ *fixtureReader, _ *PAR2ProofDomain, budget *PAR2RepairBudget, _ *CrossLaneRecover) {
				budget.MaxBytes = 1
			},
			wantErr: ErrPAR2RepairAbstain,
		},
		{
			name: "short block",
			mutate: func(_ *fixtureReader, _ *PAR2ProofDomain, _ *PAR2RepairBudget, recover *CrossLaneRecover) {
				*recover = func(context.Context, string, int, int64, PAR2Proof) (CrossLaneBlock, error) {
					return CrossLaneBlock{Data: []byte{1}}, nil
				}
			},
			wantErr: ErrCrossLaneAbstain,
		},
		{
			name: "corrupt block",
			mutate: func(_ *fixtureReader, _ *PAR2ProofDomain, _ *PAR2RepairBudget, recover *CrossLaneRecover) {
				*recover = func(_ context.Context, _ string, _ int, _ int64, proof PAR2Proof) (CrossLaneBlock, error) {
					end := proof.Offset + proof.Length
					data := append([]byte(nil), media[proof.Offset:end]...)
					data[0] ^= 0xff
					return CrossLaneBlock{Data: data}, nil
				}
			},
			wantErr: ErrCrossLaneAbstain,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, reader, _, _ := fixtureRequest(t, 1)
			proof := req.Proof
			budget := req.Budget
			recover := CrossLaneRecover(valid)
			tc.mutate(reader, &proof, &budget, &recover)
			_, err := recoverCrossLaneSpan(context.Background(), reader, "item", 0, 1, int64(len(media)), proof, budget, req.Now, recover)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error=%v want %v", err, tc.wantErr)
			}
		})
	}
}

func TestCrossLaneRecoverySucceedsBeyondPAR2Budget(t *testing.T) {
	req, reader, media, segments := fixtureRequest(t, 1)
	recover := func(_ context.Context, _ string, _ int, _ int64, proof PAR2Proof) (CrossLaneBlock, error) {
		end := proof.Offset + proof.Length
		return CrossLaneBlock{
			Data: append([]byte(nil), media[proof.Offset:end]...),
			Lane: contentproof.LaneTorrent,
		}, nil
	}
	baseline, err := recoverCrossLaneSpan(context.Background(), reader, "item", 0, 1, int64(len(media)), req.Proof, req.Budget, req.Now, recover)
	if err != nil {
		t.Fatal(err)
	}

	req.Budget.MaxBytes = baseline.spent
	_, repairReader, _, _ := fixtureRequest(t, 1)
	if _, err := RepairSegment(context.Background(), repairReader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
		t.Fatalf("PAR2 repair error=%v want %v", err, ErrPAR2RepairAbstain)
	}
	_, crossReader, _, _ := fixtureRequest(t, 1)
	got, err := recoverCrossLaneSpan(context.Background(), crossReader, "item", 0, 1, int64(len(media)), req.Proof, req.Budget, req.Now, recover)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.data, segments[1]) || got.torrentBlocks != int(got.blocks) {
		t.Fatalf("cross-lane result differs: bytes=%d counters=%+v", len(got.data), got)
	}
}

func TestRecoverCrossLaneSpanChecksBudgetBeforeAlternateRead(t *testing.T) {
	req, reader, media, _ := fixtureRequest(t, 1)
	var firstBlock, recoveredBytes int64
	valid := func(_ context.Context, _ string, _ int, _ int64, proof PAR2Proof) (CrossLaneBlock, error) {
		if firstBlock == 0 {
			firstBlock = proof.Length
		}
		recoveredBytes += proof.Length
		end := proof.Offset + proof.Length
		return CrossLaneBlock{
			Data: append([]byte(nil), media[proof.Offset:end]...),
			Lane: contentproof.LaneTorrent,
		}, nil
	}
	baseline, err := recoverCrossLaneSpan(context.Background(), reader, "item", 0, 1, int64(len(media)), req.Proof, req.Budget, req.Now, valid)
	if err != nil {
		t.Fatal(err)
	}
	measurementBytes := baseline.spent - recoveredBytes
	if measurementBytes < 0 || firstBlock <= 0 {
		t.Fatalf("invalid accounting: spent=%d recovered=%d first=%d", baseline.spent, recoveredBytes, firstBlock)
	}

	_, reader, _, _ = fixtureRequest(t, 1)
	budget := req.Budget
	budget.MaxBytes = measurementBytes + firstBlock - 1
	calls := 0
	_, err = recoverCrossLaneSpan(context.Background(), reader, "item", 0, 1, int64(len(media)), req.Proof, budget, req.Now,
		func(context.Context, string, int, int64, PAR2Proof) (CrossLaneBlock, error) {
			calls++
			return CrossLaneBlock{}, nil
		})
	if !errors.Is(err, ErrPAR2RepairAbstain) {
		t.Fatalf("error=%v want %v", err, ErrPAR2RepairAbstain)
	}
	if calls != 0 {
		t.Fatalf("alternate source called %d times after budget projection failed", calls)
	}
}

func TestRecoverCrossLaneSpanCancellationAndSlotCleanup(t *testing.T) {
	req, reader, media, _ := fixtureRequest(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := recoverCrossLaneSpan(ctx, reader, "item", 0, 1, int64(len(media)), req.Proof, req.Budget, req.Now,
		func(context.Context, string, int, int64, PAR2Proof) (CrossLaneBlock, error) {
			t.Fatal("canceled recovery called source")
			return CrossLaneBlock{}, nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}

	provider := &NNTPProvider{}
	provider.SetCrossLaneRecovery(func(context.Context, string, int, int64, PAR2Proof) (CrossLaneBlock, error) {
		return CrossLaneBlock{}, nil
	})
	provider.crossLaneSlot <- struct{}{}
	session := &par2RepairSession{provider: provider}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer waitCancel()
	_, err = session.recoverCrossLaneSegment(waitCtx, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slot cancellation error=%v", err)
	}
	if len(provider.crossLaneSlot) != 1 {
		t.Fatalf("slot leaked or released another caller's lease: len=%d", len(provider.crossLaneSlot))
	}
	<-provider.crossLaneSlot
}
