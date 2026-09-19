package imap

import (
	"errors"
	"testing"
)

func TestValidateMessageIdentity(t *testing.T) {
	tests := []struct {
		name string
		uids []uint32
		want error
	}{
		{name: "same UID", uids: []uint32{7}},
		{name: "missing", want: ErrMessageIdentityNotFound},
		{name: "UID changed", uids: []uint32{8}, want: ErrMessageIdentityChanged},
		{name: "ambiguous", uids: []uint32{7, 8}, want: ErrMessageIdentityAmbiguous},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMessageIdentity(tt.uids, 7)
			if !errors.Is(err, tt.want) {
				t.Fatalf("validateMessageIdentity(%v, 7) = %v, want %v", tt.uids, err, tt.want)
			}
		})
	}
}
