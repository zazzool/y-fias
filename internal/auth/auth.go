// Package auth — доступ к сервису по API-ключам.
//
// Через сервис проходят адреса вызовов психиатрической неотложки —
// специальная категория персональных данных (152-ФЗ, ст. 10). Закрытый порт
// в docker-сети был единственной защитой теневого режима; ключ добавляет
// вторую, не зависящую от того, как устроена сеть на конкретном сервере.
//
// Сервер хранит только SHA-256 ключей. Сам ключ живёт у клиента (в
// окружении бэкенда МИС) и в конфигурации Y-FIAS не появляется: утечка
// файла ключей сервиса не даёт доступа. Ключи генерируются случайными
// 256-битными, поэтому медленный хеш вроде bcrypt не нужен — подбирать
// нечего, а проверка остаётся дешёвой на каждом запросе.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Role — что разрешено ключу.
type Role string

const (
	// RoleClient — бэкенд МИС: подсказки, разбор, проверка, район, теневая
	// отправка, чтение флагов переключения.
	RoleClient Role = "client"
	// RoleAdmin — админка МИС: отчёты, разбор расхождений, изменение флагов.
	// Включает всё, что разрешено client и metrics.
	RoleAdmin Role = "admin"
	// RoleMetrics — сборщик метрик. Ничего, кроме /metrics.
	RoleMetrics Role = "metrics"
)

var roles = []Role{RoleClient, RoleAdmin, RoleMetrics}

// Allows сообщает, покрывает ли роль требуемую.
func (r Role) Allows(need Role) bool {
	return r == need || r == RoleAdmin
}

// KeyPrefix отличает ключ Y-FIAS от прочих секретов в окружении и логах
// сканеров утечек.
const KeyPrefix = "yf_"

// Key — опознанный ключ.
type Key struct {
	Name string
	Role Role
	hash [sha256.Size]byte
}

// Keyring — набор допустимых ключей.
type Keyring struct {
	keys []Key
}

// Generate создаёт новый ключ и строку для файла ключей сервера.
func Generate(name string, role Role) (key, line string, err error) {
	if err := validName(name); err != nil {
		return "", "", err
	}
	if !slices.Contains(roles, role) {
		return "", "", fmt.Errorf("неизвестная роль %q", role)
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	key = KeyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(key))
	return key, fmt.Sprintf("%s %s sha256:%s", name, role, hex.EncodeToString(sum[:])), nil
}

// Parse читает файл ключей: по строке «имя роль sha256:хеш».
//
// Пустые строки и строки с # пропускаются. Любая ошибка формата — отказ
// целиком, а не пропуск строки: молча потерянный ключ админки обнаружился
// бы в момент, когда нужен откат.
func Parse(text string) (*Keyring, error) {
	kr := &Keyring{}
	seen := map[string]bool{}
	for i, raw := range strings.Split(strings.ReplaceAll(text, ",", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("строка %d: ожидается «имя роль sha256:хеш»", i+1)
		}
		name, role, h := f[0], Role(f[1]), f[2]
		if err := validName(name); err != nil {
			return nil, fmt.Errorf("строка %d: %w", i+1, err)
		}
		if seen[name] {
			return nil, fmt.Errorf("строка %d: имя %q повторяется", i+1, name)
		}
		if !slices.Contains(roles, role) {
			return nil, fmt.Errorf("строка %d: неизвестная роль %q", i+1, role)
		}
		if strings.HasPrefix(h, KeyPrefix) {
			// Самая вероятная ошибка — вставить в файл сам ключ вместо
			// хеша. Ключ при этом уже скомпрометирован лежанием на сервере.
			return nil, fmt.Errorf("строка %d: в файле ключ вместо хеша; "+
				"ключ скомпрометирован, сгенерируйте новый (-gen-key)", i+1)
		}
		hexHash, ok := strings.CutPrefix(h, "sha256:")
		if !ok {
			return nil, fmt.Errorf("строка %d: хеш должен начинаться с «sha256:»", i+1)
		}
		sum, err := hex.DecodeString(hexHash)
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("строка %d: неверный sha256", i+1)
		}
		k := Key{Name: name, Role: role}
		copy(k.hash[:], sum)
		kr.keys = append(kr.keys, k)
		seen[name] = true
	}
	if len(kr.keys) == 0 {
		return nil, errors.New("ни одного ключа")
	}
	return kr, nil
}

// Len — число ключей.
func (kr *Keyring) Len() int { return len(kr.keys) }

// Lookup находит ключ.
//
// Сравниваются хеши, и всегда со всеми ключами: время ответа не зависит ни
// от того, какой ключ совпал, ни от того, совпал ли хоть один.
func (kr *Keyring) Lookup(key string) (Key, bool) {
	if !strings.HasPrefix(key, KeyPrefix) {
		return Key{}, false
	}
	sum := sha256.Sum256([]byte(key))
	var (
		found Key
		ok    bool
	)
	for _, k := range kr.keys {
		if subtle.ConstantTimeCompare(sum[:], k.hash[:]) == 1 {
			found, ok = k, true
		}
	}
	return found, ok
}

// FromRequest достаёт ключ из запроса: X-API-Key или Authorization: Bearer.
//
// Ключ в строке запроса не принимается намеренно: URL попадает в журналы
// прокси и nginx, а оттуда — куда угодно.
func FromRequest(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("X-API-Key")); k != "" {
		return k
	}
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func validName(name string) error {
	if name == "" {
		return errors.New("пустое имя ключа")
	}
	for _, r := range name {
		if !nameRune(r) {
			return fmt.Errorf("имя ключа %q: допустимы латиница, цифры и знаки «-_.»", name)
		}
	}
	return nil
}

func nameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		r == '-' || r == '_' || r == '.'
}
