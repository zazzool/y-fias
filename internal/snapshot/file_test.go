package snapshot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zazzool/y-fias/internal/index"
)

func TestWriteRead(t *testing.T) {
	streets := []index.Street{
		{Name: "Тверская", Type: "улица", Region: "77", District: "Тверской", Calls: 42},
		{Name: "Саляма Адиля", Type: "улица", Region: "77", District: "Хорошёво-Мнёвники"},
		{Name: "Вишнёвая", Type: "улица", Region: "50", District: "Раменский", Settlement: "СНТ Лесная поляна"},
		{Name: "Без типа"},
	}

	path := filepath.Join(t.TempDir(), "test.yfsnap")
	hdr, err := Write(path, streets, 20260814)
	if err != nil {
		t.Fatalf("запись: %v", err)
	}
	if hdr.Streets != int32(len(streets)) {
		t.Errorf("в заголовке улиц %d, ожидалось %d", hdr.Streets, len(streets))
	}

	got, gotHdr, err := Read(path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if gotHdr.Version != 20260814 {
		t.Errorf("версия %d, ожидалось 20260814", gotHdr.Version)
	}
	if len(got) != len(streets) {
		t.Fatalf("прочитано %d улиц, ожидалось %d", len(got), len(streets))
	}

	for i := range streets {
		w, g := streets[i], got[i]
		if w.Name != g.Name || w.Type != g.Type || w.Region != g.Region ||
			w.District != g.District || w.Settlement != g.Settlement || w.Calls != g.Calls {
			t.Errorf("улица %d разошлась:\n  записано %+v\n  прочитано %+v", i, w, g)
		}
	}
}

// Повреждённый файл обязан приводить к ошибке, а не к правдоподобному мусору:
// сервис, поднявшийся на битом снапшоте, отвечал бы уверенно и неверно.
func TestReadRejectsCorrupted(t *testing.T) {
	streets := []index.Street{{Name: "Тверская", Type: "улица", Region: "77"}}
	path := filepath.Join(t.TempDir(), "test.yfsnap")
	if _, err := Write(path, streets, 1); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		data []byte
	}{
		{"пустой", nil},
		{"чужая сигнатура", append([]byte("NOTYFIAS"), data[8:]...)},
		{"обрезан", data[:len(data)-4]},
		{"испорчен пул строк", corrupt(data, len(data)-8)},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "bad.yfsnap")
		if err := os.WriteFile(p, c.data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Read(p); err == nil {
			t.Errorf("%s: ошибки не было, а должна быть", c.name)
		}
	}
}

func corrupt(data []byte, at int) []byte {
	out := append([]byte{}, data...)
	if at >= 0 && at < len(out) {
		out[at] ^= 0xFF
	}
	return out
}

// Незавершённая запись не должна оставлять испорченный файл под рабочим
// именем: Write пишет во временный и переименовывает.
func TestWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.yfsnap")

	if _, err := Write(path, []index.Street{{Name: "Первая"}}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(path, []index.Street{{Name: "Вторая"}, {Name: "Третья"}}, 2); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("остался временный файл %s", e.Name())
		}
	}

	got, _, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "Вторая" {
		t.Errorf("файл не заменился: %+v", got)
	}
}
