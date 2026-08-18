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
	hdr, err := Write(path, Data{Streets: streets}, 20260814)
	if err != nil {
		t.Fatalf("запись: %v", err)
	}
	if hdr.Streets != int32(len(streets)) {
		t.Errorf("в заголовке улиц %d, ожидалось %d", hdr.Streets, len(streets))
	}

	data, gotHdr, err := Read(path)
	got := data.Streets
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

// Идентификаторы ГАР обязаны переживать запись и чтение: без них
// дельта-синхронизация невозможна, а обнаруживается это только при
// следующем обновлении справочника.
//
// Тест появился после того, как размер записи был увеличен под
// идентификаторы, а сама запись значений забыта: файл вырос на 20 МБ,
// всё читалось без ошибок, но идентификаторы были нулями.
func TestWriteReadKeepsGarIDs(t *testing.T) {
	streets := []index.Street{
		{ObjectID: 1409016, ParentID: 1405113, Name: "Саляма Адиля", Type: "улица", Region: "77"},
		{ObjectID: 77000, ParentID: 77, Name: "Тверская", Type: "улица", Region: "77"},
		{Name: "Без идентификаторов"},
	}
	houses := map[uint32][]index.HouseRef{
		0: {
			{ObjectID: 555001, Canonical: "2/44с1"},
			{ObjectID: 555002, Canonical: "2/44с4"},
		},
	}

	path := filepath.Join(t.TempDir(), "ids.yfsnap")
	if _, err := Write(path, Data{Streets: streets, Houses: houses}, 1); err != nil {
		t.Fatal(err)
	}

	got, _, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}

	for i, w := range streets {
		g := got.Streets[i]
		if g.ObjectID != w.ObjectID || g.ParentID != w.ParentID {
			t.Errorf("улица %d: идентификаторы %d/%d, ожидались %d/%d",
				i, g.ObjectID, g.ParentID, w.ObjectID, w.ParentID)
		}
	}

	for i, w := range houses[0] {
		g := got.Houses[0][i]
		if g.ObjectID != w.ObjectID {
			t.Errorf("дом %d: идентификатор %d, ожидался %d", i, g.ObjectID, w.ObjectID)
		}
		if g.Canonical != w.Canonical {
			t.Errorf("дом %d: номер %q, ожидался %q", i, g.Canonical, w.Canonical)
		}
	}
}

func TestWriteReadHouses(t *testing.T) {
	streets := []index.Street{
		{Name: "Саляма Адиля", Type: "улица", Region: "77"},
		{Name: "Без домов", Type: "улица", Region: "77"},
		{Name: "Только пустые", Type: "улица", Region: "77"},
	}
	houses := map[uint32][]index.HouseRef{
		0: index.HouseRefsOf("7", "7к7", "10с4", "2/44кг"),
		// Улица, у которой все номера пустые. Именно такие 14 штук
		// разошлись между записью и чтением при первом полном ингесте.
		2: index.HouseRefsOf("", ""),
	}

	path := filepath.Join(t.TempDir(), "houses.yfsnap")
	hdr, err := Write(path, Data{Streets: streets, Houses: houses}, 1)
	if err != nil {
		t.Fatalf("запись: %v", err)
	}
	if hdr.Houses != 4 {
		t.Errorf("в заголовке домов %d, ожидалось 4", hdr.Houses)
	}

	got, _, err := Read(path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}

	// Улицы без непустых домов в карте быть не должно — иначе счётчики
	// записи и чтения расходятся.
	if len(got.Houses) != 1 {
		t.Fatalf("улиц с домами %d, ожидалась 1: %+v", len(got.Houses), got.Houses)
	}

	want := []string{"7", "7к7", "10с4", "2/44кг"}
	if len(got.Houses[0]) != len(want) {
		t.Fatalf("домов %d, ожидалось %d: %v", len(got.Houses[0]), len(want), got.Houses[0])
	}
	for i, w := range want {
		if got.Houses[0][i].Canonical != w {
			t.Errorf("дом %d: %q, ожидалось %q", i, got.Houses[0][i].Canonical, w)
		}
	}
}

// Повреждённый файл обязан приводить к ошибке, а не к правдоподобному мусору:
// сервис, поднявшийся на битом снапшоте, отвечал бы уверенно и неверно.
func TestReadRejectsCorrupted(t *testing.T) {
	streets := []index.Street{{Name: "Тверская", Type: "улица", Region: "77"}}
	path := filepath.Join(t.TempDir(), "test.yfsnap")
	if _, err := Write(path, Data{Streets: streets}, 1); err != nil {
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

	if _, err := Write(path, Data{Streets: []index.Street{{Name: "Первая"}}}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(path, Data{Streets: []index.Street{{Name: "Вторая"}, {Name: "Третья"}}}, 2); err != nil {
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

	d, _, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	got := d.Streets
	if len(got) != 2 || got[0].Name != "Вторая" {
		t.Errorf("файл не заменился: %+v", got)
	}
}
