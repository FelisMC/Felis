package fileedit

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// zent is one entry of a test archive: a deflated file, or a folder when the name
// ends in "/".
type zent struct {
	name string
	body string
	mode fs.FileMode // set on the header when non-zero
	flat bool        // written without the UTF-8 flag, as old Windows tools do
	// raw writes body stored as-is under a header declaring size and crc, so a
	// test can build an archive whose entries lie about themselves.
	raw  bool
	size uint64
	crc  uint32
}

func file(name, body string) zent { return zent{name: name, body: body} }

// lie is an entry declaring size bytes while holding body.
func lie(name, body string, size uint64) zent {
	return zent{name: name, body: body, raw: true, size: size, crc: crc32.ChecksumIEEE([]byte(body))}
}

func writeZip(t *testing.T, at string, entries ...zent) {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		fh := &zip.FileHeader{Name: e.name, Method: zip.Deflate, NonUTF8: e.flat}
		if e.mode != 0 {
			fh.SetMode(e.mode)
		}
		var dst io.Writer
		var err error
		if e.raw {
			fh.Method = zip.Store
			fh.CRC32, fh.CompressedSize64, fh.UncompressedSize64 = e.crc, uint64(len(e.body)), e.size
			dst, err = w.CreateRaw(fh)
		} else {
			dst, err = w.CreateHeader(fh)
		}
		if err != nil {
			t.Fatalf("zip entry %q: %v", e.name, err)
		}
		if _, err := io.WriteString(dst, e.body); err != nil {
			t.Fatalf("zip entry %q: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(at, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tree is everything under dir: each file's content, "<dir>" for a folder and
// "-> target" for a link. Comparing two trees is how a test says "nothing
// changed", working folders included.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case rel == ".":
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "-> " + target
		case d.IsDir():
			out[rel] = "<dir>"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertSameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the tree changed:\nbefore %v\nafter  %v", before, after)
	}
}

func unzipAt(t *testing.T, root, name string, overwrite bool) Result {
	t.Helper()
	return exec(t, root, Request{Op: OpUnzip, Path: name, Overwrite: overwrite})
}

func TestUnzip(t *testing.T) {
	t.Run("extracts files and folders into the folder holding the archive", func(t *testing.T) {
		root, _ := worldRoot(t)
		if err := os.Mkdir(filepath.Join(root, "plugins"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeZip(t, filepath.Join(root, "plugins", "pack.zip"),
			zent{name: "Essentials/"},
			file("Essentials/config.yml", "locale: zh\n"),
			zent{name: "empty/"},
			file("./readme.txt", "hi"),
			file(`win\sub\a.txt`, "from windows"),
		)
		var owned []string
		prev := ownWritten
		ownWritten = func(_ *os.Root, name string) error { owned = append(owned, name); return nil }
		defer func() { ownWritten = prev }()

		res := unzipAt(t, root, "plugins/pack.zip", false)
		if res.Code != "" || res.Files != 3 || res.Bytes != int64(len("locale: zh\n")+len("hi")+len("from windows")) {
			t.Fatalf("result = %+v", res)
		}
		got := tree(t, filepath.Join(root, "plugins"))
		delete(got, "pack.zip")
		want := map[string]string{
			"Essentials": "<dir>", "Essentials/config.yml": "locale: zh\n", "empty": "<dir>",
			"readme.txt": "hi", "win": "<dir>", "win/sub": "<dir>", "win/sub/a.txt": "from windows",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("plugins/ = %v\nwant %v", got, want)
		}
		if info, _ := os.Stat(filepath.Join(root, "plugins", "readme.txt")); info.Mode().Perm() != 0o644 {
			t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
		}
		// Every folder and file is handed to the game uid, while still in the
		// working folder: 4 folders and 3 files.
		if len(owned) != 7 {
			t.Fatalf("handed to the game uid: %v, want 7", owned)
		}
		for _, o := range owned {
			if !strings.HasPrefix(o, "plugins/"+unzipTempPrefix) {
				t.Fatalf("%s was chowned outside the working folder", o)
			}
		}
	})

	t.Run("an archive named in capitals extracts", func(t *testing.T) {
		root, _ := worldRoot(t)
		writeZip(t, filepath.Join(root, "PACK.ZIP"), file("a.txt", "a"))
		if res := unzipAt(t, root, "PACK.ZIP", false); res.Code != "" || res.Files != 1 {
			t.Fatalf("result = %+v", res)
		}
		if got := mustRead(t, filepath.Join(root, "a.txt")); got != "a" {
			t.Fatalf("a.txt = %q", got)
		}
	})

	t.Run("__MACOSX is left out", func(t *testing.T) {
		root, _ := worldRoot(t)
		writeZip(t, filepath.Join(root, "mac.zip"), file("a.txt", "a"), zent{name: "__MACOSX/"}, file("__MACOSX/._a.txt", "fork"))
		if res := unzipAt(t, root, "mac.zip", false); res.Code != "" || res.Files != 1 {
			t.Fatalf("result = %+v", res)
		}
		assertAbsent(t, filepath.Join(root, "__MACOSX"))
	})

	t.Run("a name made on Chinese Windows is decoded from GBK; UTF-8 without the flag is kept", func(t *testing.T) {
		root, _ := worldRoot(t)
		gbk, err := simplifiedchinese.GBK.NewEncoder().String("存档/说明.txt")
		if err != nil {
			t.Fatal(err)
		}
		writeZip(t, filepath.Join(root, "cn.zip"),
			zent{name: gbk, body: "中文", flat: true},
			zent{name: "macOS名字.txt", body: "utf8", flat: true},
		)
		if res := unzipAt(t, root, "cn.zip", false); res.Code != "" || res.Files != 2 {
			t.Fatalf("result = %+v", res)
		}
		if got := mustRead(t, filepath.Join(root, "存档", "说明.txt")); got != "中文" {
			t.Fatalf("存档/说明.txt = %q", got)
		}
		if got := mustRead(t, filepath.Join(root, "macOS名字.txt")); got != "utf8" {
			t.Fatalf("macOS名字.txt = %q", got)
		}
	})

	t.Run("an executable entry stays executable; every other file is 0644", func(t *testing.T) {
		root, _ := worldRoot(t)
		writeZip(t, filepath.Join(root, "sh.zip"),
			zent{name: "start.sh", body: "#!/bin/sh\n", mode: 0o755},
			zent{name: "secret.txt", body: "x", mode: 0o600},
		)
		if res := unzipAt(t, root, "sh.zip", false); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
		for name, want := range map[string]fs.FileMode{"start.sh": 0o755, "secret.txt": 0o644} {
			if info, _ := os.Stat(filepath.Join(root, name)); info.Mode().Perm() != want {
				t.Errorf("%s mode = %v, want %v", name, info.Mode().Perm(), want)
			}
		}
	})

	t.Run("a name leading outside is archive_unsafe and nothing is written", func(t *testing.T) {
		for raw, entry := range map[string]string{
			"../evil.txt":      "../evil.txt",
			"a/../../evil.txt": "a/../../evil.txt",
			`..\evil.txt`:      "../evil.txt",
			"/evil.txt":        "/evil.txt",
			"C:/evil.txt":      "C:/evil.txt",
			`c:\evil.txt`:      "c:/evil.txt",
			"nul\x00.txt":      "nul\x00.txt",
			"a/..":             "a/..",
		} {
			t.Run(entry, func(t *testing.T) {
				root, outside := worldRoot(t)
				writeZip(t, filepath.Join(root, "bad.zip"), file("ok.txt", "ok"), file(raw, "evil"))
				before := tree(t, root)
				res := unzipAt(t, root, "bad.zip", true)
				if res.Code != CodeArchiveUnsafe || res.Entry != entry {
					t.Fatalf("result = %+v; want archive_unsafe naming %q", res, entry)
				}
				assertSameTree(t, before, tree(t, root))
				assertAbsent(t, filepath.Join(outside, "evil.txt"))
			})
		}
	})

	t.Run("a link entry is archive_symlink and a pipe is archive_unsafe", func(t *testing.T) {
		for _, tc := range []struct {
			mode fs.FileMode
			code string
		}{
			{fs.ModeSymlink | 0o777, CodeArchiveSymlink},
			{fs.ModeNamedPipe | 0o644, CodeArchiveUnsafe},
		} {
			root, _ := worldRoot(t)
			writeZip(t, filepath.Join(root, "odd.zip"),
				file("ok.txt", "ok"), zent{name: "odd", body: "../../outside/secret.txt", mode: tc.mode})
			before := tree(t, root)
			res := unzipAt(t, root, "odd.zip", true)
			if res.Code != tc.code || res.Entry != "odd" {
				t.Fatalf("%v: result = %+v; want %s naming odd", tc.mode, res, tc.code)
			}
			assertSameTree(t, before, tree(t, root))
		}
	})

	t.Run("a damaged or senseless archive is archive_invalid and nothing is written", func(t *testing.T) {
		for name, tc := range map[string]struct {
			entries []zent
			entry   string
		}{
			"bytes past the declared size":     {[]zent{file("ok.txt", "ok"), lie("lie.txt", "0123456789", 3)}, "lie.txt"},
			"bytes short of the declared size": {[]zent{file("ok.txt", "ok"), lie("lie.txt", "0123456789", 20)}, "lie.txt"},
			"a wrong checksum": {[]zent{file("ok.txt", "ok"),
				{name: "crc.txt", body: "0123456789", raw: true, size: 10, crc: crc32.ChecksumIEEE([]byte("0123456789")) + 1}}, "crc.txt"},
			"the same file twice":         {[]zent{file("a.txt", "1"), file("a.txt", "2")}, "a.txt"},
			"a file and a folder at once": {[]zent{file("a", "1"), file("a/b.txt", "2")}, "a"},
			"nothing in it":               {nil, ""},
			"nothing but __MACOSX":        {[]zent{file("__MACOSX/._a", "fork")}, ""},
		} {
			t.Run(name, func(t *testing.T) {
				root, _ := worldRoot(t)
				writeZip(t, filepath.Join(root, "bad.zip"), tc.entries...)
				before := tree(t, root)
				res := unzipAt(t, root, "bad.zip", true)
				if res.Code != CodeArchiveInvalid || res.Entry != tc.entry {
					t.Fatalf("result = %+v; want archive_invalid naming %q", res, tc.entry)
				}
				assertSameTree(t, before, tree(t, root))
			})
		}
		t.Run("not a zip at all", func(t *testing.T) {
			root, _ := worldRoot(t)
			if err := os.WriteFile(filepath.Join(root, "fake.zip"), []byte("hello"), 0o644); err != nil {
				t.Fatal(err)
			}
			if res := unzipAt(t, root, "fake.zip", false); res.Code != CodeArchiveInvalid {
				t.Fatalf("result = %+v; want archive_invalid", res)
			}
		})
	})

	t.Run("the archive's own path is checked", func(t *testing.T) {
		root, outside := worldRoot(t)
		if err := os.Mkdir(filepath.Join(root, "dir.zip"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeZip(t, filepath.Join(outside, "x.zip"), file("evil.txt", "evil"))
		symlink(t, outside, filepath.Join(root, "escape-link"))
		for name, code := range map[string]string{
			"server.properties":  CodeBadPath,
			"missing.zip":        CodeNotFound,
			"dir.zip":            CodeBadPath,
			"../outside/x.zip":   CodeBadPath,
			"escape-link/x.zip":  CodeBadPath,
			"config/../../x.zip": CodeBadPath,
		} {
			if res := unzipAt(t, root, name, true); res.Code != code {
				t.Errorf("%s: result = %+v; want %s", name, res, code)
			}
		}
		assertAbsent(t, filepath.Join(root, "evil.txt"))
		assertAbsent(t, filepath.Join(outside, "evil.txt"))
	})

	t.Run("files already there are listed and nothing changes without overwrite", func(t *testing.T) {
		root, _ := worldRoot(t)
		writeZip(t, filepath.Join(root, "pack.zip"),
			file("server.properties", "motd=new\n"), file("config/paper.yml", "verbose: true\n"),
			file("config/new.yml", "new"), file("fresh/x.txt", "x"))
		before := tree(t, root)
		res := unzipAt(t, root, "pack.zip", false)
		if res.Code != CodeExists || res.ConflictCount != 2 ||
			!reflect.DeepEqual(res.Conflicts, []string{"config/paper.yml", "server.properties"}) {
			t.Fatalf("result = %+v; want exists listing config/paper.yml and server.properties", res)
		}
		assertSameTree(t, before, tree(t, root))
	})

	t.Run("conflicts are named from the server's root when the archive sits in a folder", func(t *testing.T) {
		root, _ := worldRoot(t)
		writeZip(t, filepath.Join(root, "config", "pack.zip"), file("paper.yml", "verbose: true\n"))
		res := unzipAt(t, root, "config/pack.zip", false)
		if res.Code != CodeExists || !reflect.DeepEqual(res.Conflicts, []string{"config/paper.yml"}) {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("the list stops at MaxConflicts and the count does not", func(t *testing.T) {
		root, _ := worldRoot(t)
		var entries []zent
		for i := range MaxConflicts + 1 {
			name := fmt.Sprintf("f%03d.txt", i)
			if err := os.WriteFile(filepath.Join(root, name), []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, file(name, "new"))
		}
		writeZip(t, filepath.Join(root, "many.zip"), entries...)
		res := unzipAt(t, root, "many.zip", false)
		if res.Code != CodeExists || res.ConflictCount != MaxConflicts+1 || len(res.Conflicts) != MaxConflicts ||
			res.Conflicts[0] != "f000.txt" || res.Conflicts[MaxConflicts-1] != "f199.txt" {
			t.Fatalf("code %q, count %d, %d listed (%v … %v)", res.Code, res.ConflictCount, len(res.Conflicts),
				res.Conflicts[:1], res.Conflicts[len(res.Conflicts)-1:])
		}
	})

	t.Run("overwrite replaces files, merges folders and keeps everything else", func(t *testing.T) {
		root, _ := worldRoot(t)
		if err := os.WriteFile(filepath.Join(root, "config", "keep.yml"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeZip(t, filepath.Join(root, "pack.zip"),
			file("server.properties", "motd=new\n"), file("config/paper.yml", "verbose: true\n"),
			file("config/new.yml", "new"), file("fresh/x.txt", "x"))
		res := unzipAt(t, root, "pack.zip", true)
		if res.Code != "" || res.Files != 4 {
			t.Fatalf("result = %+v", res)
		}
		got := tree(t, root)
		delete(got, "pack.zip")
		want := map[string]string{
			"server.properties": "motd=new\n", "config": "<dir>", "config/paper.yml": "verbose: true\n",
			"config/keep.yml": "keep", "config/new.yml": "new", "fresh": "<dir>", "fresh/x.txt": "x",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("world = %v\nwant %v", got, want)
		}
	})

	t.Run("a file where the server has a folder, the reverse, or a link is type_conflict even with overwrite", func(t *testing.T) {
		for name, tc := range map[string]struct {
			entry string
			want  string
		}{
			"folder over a file": {"server.properties/x.txt", "server.properties"},
			"file over a folder": {"config", "config"},
			"folder over a link": {"escape-link/evil.txt", "escape-link"},
			"file over a link":   {"planted.txt", "planted.txt"},
		} {
			t.Run(name, func(t *testing.T) {
				root, outside := worldRoot(t)
				symlink(t, outside, filepath.Join(root, "escape-link"))
				symlink(t, "server.properties", filepath.Join(root, "planted.txt"))
				writeZip(t, filepath.Join(root, "pack.zip"), file("ok.txt", "ok"), file(tc.entry, "evil"))
				before := tree(t, root)
				res := unzipAt(t, root, "pack.zip", true)
				if res.Code != CodeTypeConflict || res.Entry != tc.want {
					t.Fatalf("result = %+v; want type_conflict naming %s", res, tc.want)
				}
				assertSameTree(t, before, tree(t, root))
				assertAbsent(t, filepath.Join(outside, "evil.txt"))
			})
		}
	})

	t.Run("more than the volume has free is no_space before anything is written", func(t *testing.T) {
		root, _ := worldRoot(t)
		writeZip(t, filepath.Join(root, "pack.zip"), file("d/a.txt", "0123456789"), file("d/b.txt", "0123456789"))
		need := int64(20 + 3*unzipEntryOverhead) // two files and the folder d
		stubStatfs(t, uint64(need-1), 1<<30)
		before := tree(t, root)
		res := unzipAt(t, root, "pack.zip", false)
		if res.Code != CodeNoSpace || res.Need != need || res.Avail != need-1 {
			t.Fatalf("result = %+v; want no_space with need %d, avail %d", res, need, need-1)
		}
		assertSameTree(t, before, tree(t, root))
		stubStatfs(t, uint64(need), 1<<30)
		if res := unzipAt(t, root, "pack.zip", false); res.Code != "" {
			t.Fatalf("with exactly enough room: %+v", res)
		}
	})

	t.Run("the volume filling up mid-way is no_space and changes nothing", func(t *testing.T) {
		root, _ := worldRoot(t)
		writeZip(t, filepath.Join(root, "pack.zip"), file("a.txt", "a"), file("server.properties", "motd=new\n"))
		calls := 0
		prev := syncWritten
		syncWritten = func(f *os.File) error {
			if calls++; calls == 2 {
				return syscall.ENOSPC
			}
			return f.Sync()
		}
		defer func() { syncWritten = prev }()
		before := tree(t, root)
		if res := unzipAt(t, root, "pack.zip", true); res.Code != CodeNoSpace {
			t.Fatalf("result = %+v; want no_space", res)
		}
		assertSameTree(t, before, tree(t, root))
	})

	// The renames are the one step that touches the server's files; one failing
	// part-way must put back every file already moved, replaced ones included.
	t.Run("a rename failing part-way is undone, whichever it is", func(t *testing.T) {
		// config present: paper.yml aside + in, z.yml in; zeta in; server.properties aside + in.
		const moves = 6
		for failAt := 1; failAt <= moves; failAt++ {
			root, _ := worldRoot(t)
			writeZip(t, filepath.Join(root, "pack.zip"),
				file("config/paper.yml", "verbose: true\n"), file("config/z.yml", "z"),
				file("server.properties", "motd=new\n"), file("zeta/x.txt", "x"))
			calls := 0
			prev := renameEntry
			renameEntry = func(r *os.Root, from, to string) error {
				if calls++; calls == failAt {
					return errors.New("injected rename failure")
				}
				return r.Rename(from, to)
			}
			before := tree(t, root)
			res := unzipAt(t, root, "pack.zip", true)
			renameEntry = prev
			if res.Code == "" {
				t.Fatalf("rename %d failing: result = %+v; want a failure", failAt, res)
			}
			assertSameTree(t, before, tree(t, root))
		}
		// And with none failing, the count above is the real number of moves.
		root, _ := worldRoot(t)
		writeZip(t, filepath.Join(root, "pack.zip"),
			file("config/paper.yml", "verbose: true\n"), file("config/z.yml", "z"),
			file("server.properties", "motd=new\n"), file("zeta/x.txt", "x"))
		calls := 0
		prev := renameEntry
		renameEntry = func(r *os.Root, from, to string) error { calls++; return r.Rename(from, to) }
		defer func() { renameEntry = prev }()
		if res := unzipAt(t, root, "pack.zip", true); res.Code != "" || calls != moves {
			t.Fatalf("result = %+v, %d moves; want success in %d", res, calls, moves)
		}
	})

	t.Run("a working folder a killed unzip left is cleared; a lookalike is kept", func(t *testing.T) {
		root, _ := worldRoot(t)
		stale := filepath.Join(root, unzipTempPrefix+"0123456789ab")
		if err := os.MkdirAll(filepath.Join(stale, "new"), 0o700); err != nil {
			t.Fatal(err)
		}
		// Twelve characters that are not hex, and hex a byte short or long.
		lookalikes := []string{"notahexname!", "0123456789", "0123456789abcd"}
		for _, l := range lookalikes {
			if err := os.Mkdir(filepath.Join(root, unzipTempPrefix+l), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		writeZip(t, filepath.Join(root, "pack.zip"), file("a.txt", "a"))
		if res := unzipAt(t, root, "pack.zip", false); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
		assertAbsent(t, stale)
		for _, l := range lookalikes {
			if _, err := os.Stat(filepath.Join(root, unzipTempPrefix+l)); err != nil {
				t.Fatalf("lookalike %q removed: %v", l, err)
			}
		}
	})

	t.Run("progress climbs to the total", func(t *testing.T) {
		root, _ := worldRoot(t)
		big := strings.Repeat("x", 100<<10)
		writeZip(t, filepath.Join(root, "pack.zip"), file("a.bin", big), file("b.bin", big))
		var seen []int64
		var total int64
		res := exec(t, root, Request{Op: OpUnzip, Path: "pack.zip",
			Progress: func(done, all int64) { seen = append(seen, done); total = all }})
		if res.Code != "" || total != int64(2*len(big)) || len(seen) < 2 || seen[len(seen)-1] != total {
			t.Fatalf("result = %+v; progress %d calls ending at %v of %d", res, len(seen), seen[len(seen)-1:], total)
		}
		for i := 1; i < len(seen); i++ {
			if seen[i] < seen[i-1] {
				t.Fatalf("progress went backwards: %v", seen)
			}
		}
	})
}
