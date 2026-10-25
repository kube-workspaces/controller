package controller

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestImageRepositoryFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"images/base.yaml", "images/other.yml", "images/README.md", "images/kasm/core/core.yaml", "images/kasm/desktop/app.yaml", "images/kasm/desktop/nested/ignored.yaml"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("kind: Image\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{"direct files only", []string{"images"}, []string{"images/base.yaml", "images/other.yml"}},
		{"optional trailing slash", []string{"images/"}, []string{"images/base.yaml", "images/other.yml"}},
		{"explicit subfolders", []string{"images", "images/kasm/core/", "images/kasm/desktop"}, []string{"images/base.yaml", "images/other.yml", "images/kasm/core/core.yaml", "images/kasm/desktop/app.yaml"}},
		{"duplicate paths", []string{"images", "images/"}, []string{"images/base.yaml", "images/other.yml"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := imageRepositoryFiles(root, test.paths)
			if err != nil {
				t.Fatal(err)
			}
			for i := range got {
				got[i], err = filepath.Rel(root, got[i])
				if err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
	t.Run("missing folder fails rather than returning a partial inventory", func(t *testing.T) {
		files, err := imageRepositoryFiles(root, []string{"images", "does-not-exist"})
		if err == nil || files != nil {
			t.Fatalf("got files=%v err=%v; expected an error without partial inventory", files, err)
		}
	})
}
