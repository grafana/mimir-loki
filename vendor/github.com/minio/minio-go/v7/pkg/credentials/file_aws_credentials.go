/*
 * MinIO Go Library for Amazon S3 Compatible Cloud Storage
 * Copyright 2017 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package credentials

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-ini/ini"
	"github.com/minio/minio-go/v7/internal/json"
)

// A externalProcessCredentials stores the output of a credential_process
type externalProcessCredentials struct {
	Version         int
	SessionToken    string
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string
	Expiration      time.Time
}

// A FileAWSCredentials retrieves credentials from the current user's home
// directory, and keeps track if those credentials are expired.
//
// Profile ini file example: $HOME/.aws/credentials
type FileAWSCredentials struct {
	Expiry

	// Path to the shared credentials file.
	//
	// If empty will look for "AWS_SHARED_CREDENTIALS_FILE" env variable. If the
	// env value is empty will default to current user's home directory.
	// Linux/OSX: "$HOME/.aws/credentials"
	// Windows:   "%USERPROFILE%\.aws\credentials"
	Filename string

	// AWS Profile to extract credentials from the shared credentials file. If empty
	// will default to environment variable "AWS_PROFILE" or "default" if
	// environment variable is also not set.
	Profile string

	// retrieved states if the credentials have been successfully retrieved.
	retrieved bool
}

// NewFileAWSCredentials returns a pointer to a new Credentials object
// wrapping the Profile file provider.
func NewFileAWSCredentials(filename, profile string) *Credentials {
	return New(&FileAWSCredentials{
		Filename: filename,
		Profile:  profile,
	})
}

func (p *FileAWSCredentials) retrieve() (Value, error) {
	if p.Filename == "" {
		p.Filename = os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
		if p.Filename == "" {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return Value{}, err
			}
			p.Filename = filepath.Join(homeDir, ".aws", "credentials")
		}
	}
	if p.Profile == "" {
		p.Profile = os.Getenv("AWS_PROFILE")
		if p.Profile == "" {
			p.Profile = "default"
		}
	}

	p.retrieved = false

	iniProfile, err := loadProfile(p.Filename, p.Profile)
	if err != nil {
		return Value{}, err
	}

	// Default to empty string if not found.
	id := iniProfile.Key("aws_access_key_id")
	// Default to empty string if not found.
	secret := iniProfile.Key("aws_secret_access_key")
	// Default to empty string if not found.
	token := iniProfile.Key("aws_session_token")

	// If credential_process is defined, obtain credentials by executing
	// the external process
	credentialProcess := strings.TrimSpace(iniProfile.Key("credential_process").String())
	if credentialProcess != "" {
		args := strings.Fields(credentialProcess)
		if len(args) <= 1 {
			return Value{}, errors.New("invalid credential process args")
		}
		cmd := exec.Command(args[0], args[1:]...)
		out, err := cmd.Output()
		if err != nil {
			return Value{}, err
		}
		var externalProcessCredentials externalProcessCredentials
		err = json.Unmarshal([]byte(out), &externalProcessCredentials)
		if err != nil {
			return Value{}, err
		}
		p.retrieved = true
		p.SetExpiration(externalProcessCredentials.Expiration, DefaultExpiryWindow)
		return Value{
			AccessKeyID:     externalProcessCredentials.AccessKeyID,
			SecretAccessKey: externalProcessCredentials.SecretAccessKey,
			SessionToken:    externalProcessCredentials.SessionToken,
			Expiration:      externalProcessCredentials.Expiration,
			SignerType:      SignatureV4,
		}, nil
	}
	p.retrieved = true
	return Value{
		AccessKeyID:     id.String(),
		SecretAccessKey: secret.String(),
		SessionToken:    token.String(),
		SignerType:      SignatureV4,
	}, nil
}

// Retrieve reads and extracts the shared credentials from the current
// users home directory.
func (p *FileAWSCredentials) Retrieve() (Value, error) {
	return p.retrieve()
}

// RetrieveWithCredContext is like Retrieve(), cred context is no-op for File credentials
func (p *FileAWSCredentials) RetrieveWithCredContext(_ *CredContext) (Value, error) {
	return p.retrieve()
}

// loadProfiles loads from the file pointed to by shared credentials filename for profile.
// The credentials retrieved from the profile will be returned or error. Error will be
// returned if it fails to read from the file, or the data is invalid.
func loadProfile(filename, profile string) (*ini.Section, error) {
	config, err := ini.Load(filename)
	if err != nil {
		return nil, err
	}
	iniProfile, err := config.GetSection(profile)
	if err != nil {
		return nil, err
	}
	return iniProfile, nil
}
