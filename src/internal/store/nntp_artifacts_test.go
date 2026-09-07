package store

import (
	"context"
	"testing"
)

func TestNNTPArtifactsAndOffsetsReuseAcrossItems(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()
	const key = "nzb:content"
	rar := `[{"part_num":1}]`
	zip := `[{"entry_idx":0}]`

	if _, err := s.execWrite(ctx, `INSERT INTO segment_offsets
		(item_id, file_index, seg_index, decoded_bytes) VALUES ('item-a', 0, 0, 100)`); err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteNNTPArtifacts(ctx, key, "item-a", &rar, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteNNTPArtifacts(ctx, key, "item-b", nil, &zip); err != nil {
		t.Fatal(err)
	}
	artifacts, err := s.GetNNTPArtifacts(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if artifacts.RARManifest == nil || *artifacts.RARManifest != rar || artifacts.ZIPManifest == nil || *artifacts.ZIPManifest != zip {
		t.Fatalf("artifacts=%+v", artifacts)
	}

	offsets, recorded, err := s.GetSegmentOffsets(ctx, "item-a", key, 0, 1, []int64{110})
	if err != nil {
		t.Fatal(err)
	}
	if len(offsets) != 2 || offsets[1] != 100 || len(recorded) != 1 {
		t.Fatalf("shared offsets=%v recorded=%v", offsets, recorded)
	}

	if _, err := s.recordSegmentSizes(ctx, []segmentOffsetRecord{
		{itemID: "item-b", contentKey: key, fileIndex: 0, segIndex: 0, decodedBytes: 100},
		{itemID: "item-b", contentKey: key, fileIndex: 0, segIndex: 1, decodedBytes: 90},
	}); err != nil {
		t.Fatal(err)
	}
	offsets, recorded, err = s.GetSegmentOffsets(ctx, "item-c", key, 0, 2, []int64{110, 110})
	if err != nil || len(offsets) != 3 || offsets[2] != 190 || len(recorded) != 2 {
		t.Fatalf("cross-item offsets=%v recorded=%v err=%v", offsets, recorded, err)
	}
	var itemRows, sharedRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM segment_offsets WHERE item_id='item-b'`).Scan(&itemRows); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM nntp_segment_offsets WHERE content_key=?`, key).Scan(&sharedRows); err != nil {
		t.Fatal(err)
	}
	if itemRows != 2 || sharedRows != 2 {
		t.Fatalf("item rows=%d shared rows=%d", itemRows, sharedRows)
	}
}
