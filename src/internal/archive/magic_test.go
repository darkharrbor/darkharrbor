package archive

import "testing"

func TestClassifyMagic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data []byte
		want Format
	}{
		{"rar4", []byte("Rar!\x1a\x07\x00rest"), FormatRAR},
		{"rar5", []byte("Rar!\x1a\x07\x01\x00rest"), FormatRAR},
		{"rar5 truncated", []byte("Rar!\x1a\x07\x01"), FormatUnknown},
		{"rar variant unknown", []byte("Rar!\x1a\x07\x02rest"), FormatUnknown},
		{"7z", []byte("7z\xbc\xaf\x27\x1crest"), Format7z},
		{"zip", []byte("PK\x03\x04rest"), FormatZIP},
		{"mkv", []byte("\x1a\x45\xdf\xa3rest"), FormatMKV},
		{"mp4", []byte("\x00\x00\x00\x18ftypisom"), FormatMP4},
		{"mp4 short", []byte("\x00\x00\x00\x08ftyp"), FormatUnknown},
		{"empty", nil, FormatUnknown},
		{"partial zip", []byte("PK\x03"), FormatUnknown},
		{"leading junk", []byte("junkRar!\x1a\x07\x00"), FormatUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyMagic(test.data); got != test.want {
				t.Fatalf("ClassifyMagic(%x) = %q, want %q", test.data, got, test.want)
			}
		})
	}
}

func FuzzClassifyMagic(f *testing.F) {
	for _, seed := range [][]byte{
		{},
		[]byte("Rar!\x1a\x07\x00"),
		[]byte("Rar!\x1a\x07\x01\x00"),
		[]byte("7z\xbc\xaf\x27\x1c"),
		[]byte("PK\x03\x04"),
		[]byte("\x1a\x45\xdf\xa3"),
		[]byte("\x00\x00\x00\x18ftypisom"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		switch ClassifyMagic(data) {
		case FormatUnknown, FormatRAR, Format7z, FormatZIP, FormatMKV, FormatMP4:
		default:
			t.Fatal("classifier returned an invalid format")
		}
	})
}
