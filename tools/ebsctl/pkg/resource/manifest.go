package resource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxManifestBytes = 16 << 20

type Manifest struct {
	Definition Definition
	Name       string
	Project    string
	Data       []byte
	Object     map[string]any
	Source     string
}

func ReadManifests(path, project string, validate bool, stdin io.Reader) ([]Manifest, error) {
	if path == "-" {
		return decodeManifests(stdin, "stdin", project, validate)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read manifests: %w", err)
	}
	if !info.IsDir() {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		return decodeManifests(file, path, project, validate)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension == ".yaml" || extension == ".yml" || extension == ".json" {
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}
	sort.Strings(files)
	var manifests []Manifest
	for _, fileName := range files {
		file, err := os.Open(fileName)
		if err != nil {
			return nil, err
		}
		decoded, decodeErr := decodeManifests(file, fileName, project, validate)
		file.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		manifests = append(manifests, decoded...)
	}
	return manifests, nil
}

func decodeManifests(reader io.Reader, source, project string, validate bool) ([]Manifest, error) {
	limited := io.LimitReader(reader, maxManifestBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", source, maxManifestBytes)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var manifests []Manifest
	for document := 1; ; document++ {
		var node any
		err := decoder.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s document %d: %w", source, document, err)
		}
		if node == nil {
			continue
		}
		jsonData, err := json.Marshal(node)
		if err != nil {
			return nil, fmt.Errorf("%s document %d: convert YAML: %w", source, document, err)
		}
		var object map[string]any
		jsonDecoder := json.NewDecoder(bytes.NewReader(jsonData))
		jsonDecoder.UseNumber()
		if err := jsonDecoder.Decode(&object); err != nil || object == nil {
			return nil, fmt.Errorf("%s document %d: object is required", source, document)
		}
		apiVersion, _ := object["apiVersion"].(string)
		kind, _ := object["kind"].(string)
		definition, err := ResolveGVK(apiVersion, kind)
		if err != nil {
			return nil, fmt.Errorf("%s document %d: %w", source, document, err)
		}
		metadata, ok := object["metadata"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s document %d: metadata is required", source, document)
		}
		name, _ := metadata["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("%s document %d: metadata.name is required", source, document)
		}
		objectProject, _ := metadata["namespace"].(string)
		if definition.Namespaced {
			if project != "" && objectProject != "" && objectProject != project {
				return nil, fmt.Errorf("%s document %d: metadata.namespace %q conflicts with Project %q", source, document, objectProject, project)
			}
			if objectProject == "" {
				objectProject = project
				if objectProject != "" {
					metadata["namespace"] = objectProject
				}
			}
			if objectProject == "" {
				return nil, fmt.Errorf("%s document %d: %s requires a Project", source, document, definition.Kind)
			}
		} else if objectProject != "" {
			return nil, fmt.Errorf("%s document %d: Project cannot set metadata.namespace", source, document)
		}
		jsonData, err = json.Marshal(object)
		if err != nil {
			return nil, err
		}
		if validate {
			if err := StrictDecode(definition, jsonData); err != nil {
				return nil, fmt.Errorf("%s document %d: %w", source, document, err)
			}
		}
		manifests = append(manifests, Manifest{Definition: definition, Name: name, Project: objectProject, Data: jsonData, Object: object, Source: source})
	}
	if len(manifests) == 0 {
		return nil, fmt.Errorf("%s contains no objects", source)
	}
	return manifests, nil
}

func HasResourceVersion(manifest Manifest) bool {
	metadata, _ := manifest.Object["metadata"].(map[string]any)
	value, _ := metadata["resourceVersion"].(string)
	return value != ""
}
