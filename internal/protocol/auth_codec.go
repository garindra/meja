package protocol

import (
	"fmt"
	"time"
)

func EncodeAuthBegin(dst []byte, msg AuthBegin) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.Username)
	w.String(msg.PublicKey)
	return w.Buf, nil
}

func DecodeAuthBegin(payload []byte) (AuthBegin, error) {
	r := PayloadReader{Data: payload}
	username, err := r.String(MaxStringLen)
	if err != nil {
		return AuthBegin{}, fmt.Errorf("decode AuthBegin: %w", err)
	}
	publicKey, err := r.String(MaxBytesLen)
	if err != nil {
		return AuthBegin{}, fmt.Errorf("decode AuthBegin: %w", err)
	}
	if err := r.Done(); err != nil {
		return AuthBegin{}, fmt.Errorf("decode AuthBegin: %w", err)
	}
	return AuthBegin{Username: username, PublicKey: publicKey}, nil
}

func EncodeAuthChallenge(dst []byte, msg AuthChallenge) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.ChallengeID)
	w.String(msg.Nonce)
	w.Varint(msg.ExpiresAt.UnixNano())
	return w.Buf, nil
}

func DecodeAuthChallenge(payload []byte) (AuthChallenge, error) {
	r := PayloadReader{Data: payload}
	challengeID, err := r.String(MaxStringLen)
	if err != nil {
		return AuthChallenge{}, fmt.Errorf("decode AuthChallenge: %w", err)
	}
	nonce, err := r.String(MaxStringLen)
	if err != nil {
		return AuthChallenge{}, fmt.Errorf("decode AuthChallenge: %w", err)
	}
	expiresAt, err := r.Varint()
	if err != nil {
		return AuthChallenge{}, fmt.Errorf("decode AuthChallenge: %w", err)
	}
	if err := r.Done(); err != nil {
		return AuthChallenge{}, fmt.Errorf("decode AuthChallenge: %w", err)
	}
	return AuthChallenge{
		ChallengeID: challengeID,
		Nonce:       nonce,
		ExpiresAt:   time.Unix(0, expiresAt).UTC(),
	}, nil
}

func EncodeAuthResponse(dst []byte, msg AuthResponse) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.ChallengeID)
	w.Bytes(msg.Signature)
	return w.Buf, nil
}

func DecodeAuthResponse(payload []byte) (AuthResponse, error) {
	r := PayloadReader{Data: payload}
	challengeID, err := r.String(MaxStringLen)
	if err != nil {
		return AuthResponse{}, fmt.Errorf("decode AuthResponse: %w", err)
	}
	signature, err := r.Bytes(MaxBytesLen)
	if err != nil {
		return AuthResponse{}, fmt.Errorf("decode AuthResponse: %w", err)
	}
	if err := r.Done(); err != nil {
		return AuthResponse{}, fmt.Errorf("decode AuthResponse: %w", err)
	}
	return AuthResponse{ChallengeID: challengeID, Signature: append([]byte(nil), signature...)}, nil
}

func EncodeAuthOK(dst []byte, msg AuthOK) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.Username)
	w.String(msg.HomeDir)
	w.String(msg.Shell)
	return w.Buf, nil
}

func DecodeAuthOK(payload []byte) (AuthOK, error) {
	r := PayloadReader{Data: payload}
	username, err := r.String(MaxStringLen)
	if err != nil {
		return AuthOK{}, fmt.Errorf("decode AuthOK: %w", err)
	}
	homeDir, err := r.String(MaxStringLen)
	if err != nil {
		return AuthOK{}, fmt.Errorf("decode AuthOK: %w", err)
	}
	shell, err := r.String(MaxStringLen)
	if err != nil {
		return AuthOK{}, fmt.Errorf("decode AuthOK: %w", err)
	}
	if err := r.Done(); err != nil {
		return AuthOK{}, fmt.Errorf("decode AuthOK: %w", err)
	}
	return AuthOK{Username: username, HomeDir: homeDir, Shell: shell}, nil
}

func EncodeAuthFailed(dst []byte, msg AuthFailed) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.Reason)
	return w.Buf, nil
}

func DecodeAuthFailed(payload []byte) (AuthFailed, error) {
	r := PayloadReader{Data: payload}
	reason, err := r.String(MaxStringLen)
	if err != nil {
		return AuthFailed{}, fmt.Errorf("decode AuthFailed: %w", err)
	}
	if err := r.Done(); err != nil {
		return AuthFailed{}, fmt.Errorf("decode AuthFailed: %w", err)
	}
	return AuthFailed{Reason: reason}, nil
}
