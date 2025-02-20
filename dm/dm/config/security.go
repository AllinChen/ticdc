// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"fmt"
	"os"
	"path"

	"github.com/pingcap/ticdc/dm/pkg/utils"
)

// Security config.
type Security struct {
	SSLCA         string   `toml:"ssl-ca" json:"ssl-ca" yaml:"ssl-ca"`
	SSLCert       string   `toml:"ssl-cert" json:"ssl-cert" yaml:"ssl-cert"`
	SSLKey        string   `toml:"ssl-key" json:"ssl-key" yaml:"ssl-key"`
	CertAllowedCN strArray `toml:"cert-allowed-cn" json:"cert-allowed-cn" yaml:"cert-allowed-cn"`
	SSLCABytes    []byte   `toml:"ssl-ca-bytes" json:"-" yaml:"ssl-ca-bytes"`
	SSLKEYBytes   []byte   `toml:"ssl-key-bytes" json:"-" yaml:"ssl-key-bytes"`
	SSLCertBytes  []byte   `toml:"ssl-cert-bytes" json:"-" yaml:"ssl-cert-bytes"`
}

// used for parse string slice in flag.
type strArray []string

func (i *strArray) String() string {
	return fmt.Sprint([]string(*i))
}

func (i *strArray) Set(value string) error {
	*i = append(*i, value)
	return nil
}

// LoadTLSContent load all tls config from file.
func (s *Security) LoadTLSContent() error {
	if len(s.SSLCABytes) > 0 {
		// already loaded
		return nil
	}

	if s.SSLCA != "" {
		dat, err := os.ReadFile(s.SSLCA)
		if err != nil {
			return err
		}
		s.SSLCABytes = dat
	}
	if s.SSLCert != "" {
		dat, err := os.ReadFile(s.SSLCert)
		if err != nil {
			return err
		}
		s.SSLCertBytes = dat
	}
	if s.SSLKey != "" {
		dat, err := os.ReadFile(s.SSLKey)
		if err != nil {
			return err
		}
		s.SSLKEYBytes = dat
	}
	return nil
}

// DumpTLSContent dump tls certs data to file.
// if user specified the path for certs but the cert doesn't exist or user didn't specify the path for certs
// dump certs to dm-worker folder and change the cert path.
// see more here https://github.com/pingcap/ticdc/pull/3260#discussion_r749052994
func (s *Security) DumpTLSContent(baseDirPath string) error {
	if s.SSLCA == "" || !utils.IsFileExists(s.SSLCA) {
		s.SSLCA = path.Join(baseDirPath, "ca.pem")
		if err := utils.WriteFileAtomic(s.SSLCA, s.SSLCABytes, 0o600); err != nil {
			return err
		}
	}
	if s.SSLCert == "" || !utils.IsFileExists(s.SSLCert) {
		s.SSLCert = path.Join(baseDirPath, "cert.pem")
		if err := utils.WriteFileAtomic(s.SSLCert, s.SSLCertBytes, 0o600); err != nil {
			return err
		}
	}
	if s.SSLKey == "" || !utils.IsFileExists(s.SSLKey) {
		s.SSLKey = path.Join(baseDirPath, "key.pem")
		if err := utils.WriteFileAtomic(s.SSLKey, s.SSLKEYBytes, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// ClearSSLBytesData clear all tls config bytes data.
func (s *Security) ClearSSLBytesData() {
	s.SSLCABytes = s.SSLCABytes[:0]
	s.SSLKEYBytes = s.SSLKEYBytes[:0]
	s.SSLCertBytes = s.SSLCertBytes[:0]
}

// Clone returns a deep copy of Security.
func (s *Security) Clone() *Security {
	if s == nil {
		return nil
	}
	clone := *s
	clone.CertAllowedCN = append(strArray(nil), s.CertAllowedCN...)
	clone.SSLCABytes = append([]byte(nil), s.SSLCABytes...)
	clone.SSLKEYBytes = append([]byte(nil), s.SSLKEYBytes...)
	clone.SSLCertBytes = append([]byte(nil), s.SSLCertBytes...)
	return &clone
}
