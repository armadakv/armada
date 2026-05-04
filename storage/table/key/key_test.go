// Copyright JAMF Software, LLC

package key

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecoder_Decode(t *testing.T) {
	type fields struct {
		r io.Reader
	}
	tests := []struct {
		name    string
		fields  fields
		wantErr bool
		wantKey Key
	}{
		{
			name: "Decode - V2 User key with seqno=1",
			// header [0x02 0x00 0x00 0x00] + keyType(User) + "test" + 0x00 separator + encoded seqno=1 [FF FF FF FF FF FF FF FE]
			fields: fields{r: bytes.NewBuffer(append(append([]byte{V2, 0x0, 0x0, 0x0, byte(TypeUser)}, append([]byte("test"), 0x00)...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe))},
			wantKey: Key{
				version: V2,
				KeyType: TypeUser,
				Key:     []byte("test"),
				Seqno:   1,
			},
		},
		{
			name: "Decode - V2 User key with seqno=0",
			// seqno=0 encoded as all 0xFF; null separator before seqno
			fields: fields{r: bytes.NewBuffer(append(append([]byte{V2, 0x0, 0x0, 0x0, byte(TypeUser)}, append([]byte("test"), 0x00)...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff))},
			wantKey: Key{
				version: V2,
				KeyType: TypeUser,
				Key:     []byte("test"),
				Seqno:   0,
			},
		},
		{
			name:   "Decode - V1 System key",
			fields: fields{r: bytes.NewBuffer(append([]byte{V1, 0x0, 0x0, 0x0, byte(TypeSystem)}, []byte("test")...))},
			wantKey: Key{
				version: V1,
				KeyType: TypeSystem,
				Key:     []byte("test"),
			},
		},
		{
			name:   "Decode - V1 User key",
			fields: fields{r: bytes.NewBuffer(append([]byte{V1, 0x0, 0x0, 0x0, byte(TypeUser)}, []byte("test")...))},
			wantKey: Key{
				version: V1,
				KeyType: TypeUser,
				Key:     []byte("test"),
			},
		},
		{
			name:    "Decode - Malformed header",
			fields:  fields{r: bytes.NewBuffer(append([]byte{0x0, 0x0, byte(TypeUser)}, []byte("test")...))},
			wantErr: true,
		},
		{
			name:    "Decode - Missing header",
			fields:  fields{r: bytes.NewBuffer([]byte{0x0, 0x0})},
			wantErr: true,
		},
		{
			name:    "Decode - Unknown Key Version",
			fields:  fields{r: bytes.NewBuffer(append([]byte{UnknownVersion, 0x0, 0x0, 0x0, byte(TypeUser)}, []byte("test")...))},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			d := NewDecoder(tt.fields.r)

			k := Key{}
			err := d.Decode(&k)
			if tt.wantErr {
				r.Error(err)
				return
			}
			r.NoError(err)
			r.Equal(tt.wantKey, k)
		})
	}
}

func TestEncoder_Encode(t *testing.T) {
	type fields struct {
		w io.Writer
	}
	type args struct {
		key *Key
	}
	tests := []struct {
		name       string
		fields     fields
		args       args
		wantErr    bool
		wantWriter []byte
	}{
		{
			name:   "Encode - V1 System key",
			fields: fields{w: bytes.NewBuffer(make([]byte, 0, V1KeyLen))},
			args: args{key: &Key{
				version: V1,
				KeyType: TypeSystem,
				Key:     []byte("test"),
			}},
			wantWriter: append([]byte{V1, 0x0, 0x0, 0x0, byte(TypeSystem)}, []byte("test")...),
		},
		{
			name:   "Encode - V1 User key",
			fields: fields{w: bytes.NewBuffer(make([]byte, 0, V1KeyLen))},
			args: args{key: &Key{
				version: V1,
				KeyType: TypeUser,
				Key:     []byte("test"),
			}},
			wantWriter: append([]byte{V1, 0x0, 0x0, 0x0, byte(TypeUser)}, []byte("test")...),
		},
		{
			name:   "Encode - V1 System key - small buffer",
			fields: fields{w: bytes.NewBuffer(make([]byte, 0, 1))},
			args: args{key: &Key{
				version: V1,
				KeyType: TypeUser,
				Key:     []byte("test"),
			}},
			wantWriter: append([]byte{V1, 0x0, 0x0, 0x0, byte(TypeUser)}, []byte("test")...),
		},
		{
			name:   "Encode - Auto-pick Latest Key Version",
			fields: fields{w: bytes.NewBuffer(make([]byte, 0, 1))},
			args: args{key: &Key{
				KeyType: TypeUser,
				Key:     []byte("test"),
			}},
			// LatestVersion is now V2: header [0x02 0x00 0x00 0x00] + keyType + userKey + 0x00 sep + seqno(8B).
			// seqno=0 → ^uint64(0)=MaxUint64 big-endian → [FF FF FF FF FF FF FF FF].
			wantWriter: append(append([]byte{V2, 0x0, 0x0, 0x0, byte(TypeUser)}, append([]byte("test"), 0x00)...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff),
		},
		{
			name:   "Encode - V2 User key with seqno",
			fields: fields{w: bytes.NewBuffer(make([]byte, 0, V2KeyLen))},
			args: args{key: &Key{
				version: V2,
				KeyType: TypeUser,
				Key:     []byte("test"),
				Seqno:   1,
			}},
			// seqno=1 → ^uint64(1)=0xFFFFFFFFFFFFFFFE big-endian → [FF FF FF FF FF FF FF FE].
			// null separator (0x00) sits between user key and seqno.
			wantWriter: append(append([]byte{V2, 0x0, 0x0, 0x0, byte(TypeUser)}, append([]byte("test"), 0x00)...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe),
		},
		{
			name:   "Encode - V2 System key",
			fields: fields{w: bytes.NewBuffer(make([]byte, 0, V2KeyLen))},
			args: args{key: &Key{
				version: V2,
				KeyType: TypeSystem,
				Key:     []byte("index"),
				Seqno:   0,
			}},
			// null separator (0x00) sits between user key and seqno.
			wantWriter: append(append([]byte{V2, 0x0, 0x0, 0x0, byte(TypeSystem)}, append([]byte("index"), 0x00)...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			e := NewEncoder(tt.fields.w)

			n, err := e.Encode(tt.args.key)
			if tt.wantErr {
				r.Error(err)
				return
			}
			r.NoError(err)
			r.Equal(tt.wantWriter, tt.fields.w.(*bytes.Buffer).Bytes()[:n])
		})
	}
}

func TestKey_reset(t *testing.T) {
	type fields struct {
		version uint8
		KeyType Type
		Key     []byte
	}
	tests := []struct {
		name   string
		fields fields
	}{
		{
			name: "Reset - V1 System key",
			fields: fields{
				version: V1,
				KeyType: TypeSystem,
				Key:     []byte("test"),
			},
		},
		{
			name: "Reset - V1 User key",
			fields: fields{
				version: V1,
				KeyType: TypeUser,
				Key:     []byte("test"),
			},
		},
		{
			name:   "Reset - Empty key",
			fields: fields{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			k := &Key{
				version: tt.fields.version,
				KeyType: tt.fields.KeyType,
				Key:     tt.fields.Key,
			}
			k.reset()

			r.Empty(k.version)
			r.Empty(k.KeyType)
			r.Empty(k.Key)
		})
	}
}

func TestDecodeRaw(t *testing.T) {
	type args struct {
		raw []byte
	}
	tests := []struct {
		name    string
		args    args
		want    Key
		wantErr require.ErrorAssertionFunc
	}{
		{
			name: "v1 system key",
			args: args{raw: append([]byte{V1, 0x0, 0x0, 0x0, byte(TypeSystem)}, []byte("test")...)},
			want: Key{
				version: V1,
				KeyType: TypeSystem,
				Key:     []byte("test"),
			},
			wantErr: require.NoError,
		},
		{
			name: "v1 User key",
			args: args{raw: append([]byte{V1, 0x0, 0x0, 0x0, byte(TypeUser)}, []byte("test")...)},
			want: Key{
				version: V1,
				KeyType: TypeUser,
				Key:     []byte("test"),
			},
			wantErr: require.NoError,
		},
		{
			name: "v2 User key with seqno=1",
			// null separator (0x00) between user key and seqno
			args: args{raw: append(append([]byte{V2, 0x0, 0x0, 0x0, byte(TypeUser)}, append([]byte("test"), 0x00)...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe)},
			want: Key{
				version: V2,
				KeyType: TypeUser,
				Key:     []byte("test"),
				Seqno:   1,
			},
			wantErr: require.NoError,
		},
		{
			name: "v2 System key with seqno=0",
			// null separator (0x00) between user key and seqno
			args: args{raw: append(append([]byte{V2, 0x0, 0x0, 0x0, byte(TypeSystem)}, append([]byte("index"), 0x00)...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)},
			want: Key{
				version: V2,
				KeyType: TypeSystem,
				Key:     []byte("index"),
				Seqno:   0,
			},
			wantErr: require.NoError,
		},
		{
			name:    "malformed header",
			args:    args{raw: append([]byte{0x0, 0x0, byte(TypeUser)}, []byte("test")...)},
			wantErr: require.Error,
		},
		{
			name:    "missing header",
			args:    args{raw: []byte{0x0, 0x0}},
			wantErr: require.Error,
		},
		{
			name:    "unknown Key Version",
			args:    args{raw: append([]byte{UnknownVersion, 0x0, 0x0, 0x0, byte(TypeUser)}, []byte("test")...)},
			wantErr: require.Error,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeBytes(tt.args.raw)
			tt.wantErr(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
