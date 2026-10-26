package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// The public trial has its own account file and never reads the owner's accounts.
const maxTrialAccounts = 2000

var (
	errAccountExists   = errors.New("ce nom est déjà utilisé")
	errAccountLimit    = errors.New("les inscriptions sont momentanément fermées")
	errInvalidUsername = errors.New("choisissez un identifiant de 3 à 32 caractères : lettres, chiffres, tiret ou soulignement")
	errWeakPassword    = errors.New("utilisez un mot de passe de 10 à 72 octets")
)

type trialAccount struct {
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

type accountFile struct {
	Version  int                     `json:"version"`
	Accounts map[string]trialAccount `json:"accounts"`
}

type accountStore struct {
	mu   sync.Mutex
	path string
	data accountFile
}

func normalizeAccountName(username string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(username))
	if len(name) < 3 || len(name) > 32 {
		return "", errInvalidUsername
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if index > 0 && (character == '-' || character == '_') {
			continue
		}
		return "", errInvalidUsername
	}
	switch name {
	case "admin", "administrator", "autodocker", "dockertracer", "root", "support", "system":
		return "", errInvalidUsername
	}
	return name, nil
}

func newAccountStore(path string) (*accountStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("AUTODOCKER_TRIAL_ACCOUNTS_FILE required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	store := &accountStore{path: path, data: accountFile{Version: 1, Accounts: map[string]trialAccount{}}}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return nil, errors.New("invalid trial account file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("trial account file permissions must be 0600")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(contents, &store.data); err != nil {
		return nil, fmt.Errorf("invalid trial account file: %w", err)
	}
	if store.data.Version != 1 || store.data.Accounts == nil || len(store.data.Accounts) > maxTrialAccounts {
		return nil, errors.New("unsupported trial account file")
	}
	return store, nil
}

// create returns the normalized account name for the authenticated session.
func (store *accountStore) create(username, password string) (string, error) {
	name, err := normalizeAccountName(username)
	if err != nil {
		return "", err
	}
	if len(password) < 10 || len(password) > 72 {
		return "", errWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.data.Accounts[name]; exists {
		return "", errAccountExists
	}
	if len(store.data.Accounts) >= maxTrialAccounts {
		return "", errAccountLimit
	}
	store.data.Accounts[name] = trialAccount{PasswordHash: string(hash), CreatedAt: time.Now().UTC()}
	if err := store.save(); err != nil {
		delete(store.data.Accounts, name)
		return "", err
	}
	return name, nil
}

// authenticate never discloses whether the username or password was wrong.
func (store *accountStore) authenticate(username, password string) (string, bool) {
	name, err := normalizeAccountName(username)
	if err != nil || len(password) > 72 {
		return "", false
	}
	store.mu.Lock()
	account, exists := store.data.Accounts[name]
	store.mu.Unlock()
	if !exists || bcrypt.CompareHashAndPassword([]byte(account.PasswordHash), []byte(password)) != nil {
		return "", false
	}
	return name, true
}

func (store *accountStore) exists(username string) bool {
	name, err := normalizeAccountName(username)
	if err != nil {
		return false
	}
	store.mu.Lock()
	_, found := store.data.Accounts[name]
	store.mu.Unlock()
	return found
}

// save is called with the account store lock. Rename keeps a complete old file
// if writing fails and the private temporary file has the final file's mode.
func (store *accountStore) save() error {
	contents, err := json.Marshal(store.data)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(store.path), ".accounts-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), store.path)
}
