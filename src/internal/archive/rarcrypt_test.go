package archive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

const rarFixturePassword = "ns33-fixture"

func loadRAR5SplitFixture(t *testing.T) ([]StoredRARMember, [][]byte) {
	t.Helper()
	names, err := filepath.Glob("testdata/ns33-rar5/archive.part*.rar")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if len(names) != 4 {
		t.Fatalf("fixture volumes = %d, want 4", len(names))
	}
	members := make([]StoredRARMember, 0, len(names))
	volumes := make([][]byte, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		member, err := ParseStoredRARMember(context.Background(), bytesource.NewMemSource("fixture", data))
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, member)
		volumes = append(volumes, data)
	}
	return members, volumes
}

func TestRAR5EncryptedSplitFixtureAndSeek(t *testing.T) {
	t.Parallel()
	members, volumes := loadRAR5SplitFixture(t)
	var ciphertext []byte
	var packed int64
	for i, member := range members {
		if member.Encryption == nil || member.Encryption.Version != 5 || member.DataSize != 2500 {
			t.Fatalf("member %d = %+v", i, member)
		}
		if i > 0 && (!bytes.Equal(member.Encryption.Salt, members[0].Encryption.Salt) ||
			!bytes.Equal(member.Encryption.IV, members[0].Encryption.IV) ||
			!bytes.Equal(member.Encryption.PasswordCheck, members[0].Encryption.PasswordCheck) ||
			member.Encryption.KDFLog2 != members[0].Encryption.KDFLog2) {
			t.Fatalf("volume %d encryption parameters differ", i)
		}
		packed += member.PackedSize
		ciphertext = append(ciphertext, volumes[i][member.DataOffset:member.DataOffset+member.PackedSize]...)
	}
	if packed != 2512 || packed%16 != 0 {
		t.Fatalf("packed total = %d, want 2512", packed)
	}

	key, err := DeriveRARKey(context.Background(), rarFixturePassword, members[0].Encryption)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := make([]byte, len(ciphertext))
	if err := key.DecryptBlocks(plaintext, ciphertext, key.InitialIV()); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 2500)
	copy(want, []byte{0x1a, 0x45, 0xdf, 0xa3})
	if !bytes.Equal(plaintext[:len(want)], want) {
		t.Fatal("full split-volume plaintext mismatch")
	}

	const start, end = int64(997), int64(2053)
	alignedStart := start &^ 15
	alignedEnd := (end + 16) &^ 15
	iv := ciphertext[alignedStart-16 : alignedStart]
	seekPlain := make([]byte, alignedEnd-alignedStart)
	if err := key.DecryptBlocks(seekPlain, ciphertext[alignedStart:alignedEnd], iv); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seekPlain[start-alignedStart:end+1-alignedStart], want[start:end+1]) {
		t.Fatal("seek plaintext mismatch")
	}

	head, err := DecryptRARPrefix(context.Background(), bytesource.NewMemSource("fixture", volumes[0]), members[0], rarFixturePassword, 16)
	if err != nil {
		t.Fatal(err)
	}
	if ClassifyMagic(head) != FormatMKV {
		t.Fatalf("prefix format = %q, want MKV", ClassifyMagic(head))
	}
}

func TestRAR5PasswordAndCancellationFailClosed(t *testing.T) {
	t.Parallel()
	members, _ := loadRAR5SplitFixture(t)
	if _, err := DeriveRARKey(context.Background(), "", members[0].Encryption); !errors.Is(err, ErrRARPasswordRequired) {
		t.Fatalf("missing password error = %v", err)
	}
	if _, err := DeriveRARKey(context.Background(), "wrong-fixture-value", members[0].Encryption); !errors.Is(err, ErrRARBadPassword) {
		t.Fatalf("wrong password error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DeriveRARKey(ctx, rarFixturePassword, members[0].Encryption); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}
