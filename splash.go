package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var httpClient = &http.Client{}

// Flags
var (
	platform           string
	manifestID         string
	installPath        string
	cachePath          string
	fileFilter         string
	downloadURLs       []string
	skipIntegrityCheck bool
)

const defaultDownloadURL = "http://epicgames-download1.akamaized.net"

func init() {
	// Seed random
	rand.Seed(time.Now().Unix())

	// Parse flags
	flag.StringVar(&platform, "platform", "Windows", "platform to download for")
	flag.StringVar(&manifestID, "manifest", "", "download a specific manifest")
	flag.StringVar(&installPath, "install-dir", "files", "install path")
	flag.StringVar(&cachePath, "cache", "cache", "cache path")
	flag.StringVar(&fileFilter, "files", "", "only download specific files")
	dlUrls := flag.String("url", defaultDownloadURL, "download url")
	flag.BoolVar(&skipIntegrityCheck, "skipcheck", false, "skip file integrity check")
	flag.Parse()

	downloadURLs = strings.Split(*dlUrls, ",")
}

func main() {
	// Make working directories
	os.MkdirAll(cachePath, os.ModePerm)

	var catalog *Catalog
	var manifest *Manifest

	// Load catalog
	catalogCachePath := filepath.Join(cachePath, "catalog.json")
	if _, err := os.Stat(catalogCachePath); err == nil { // read catalog from cache
		log.Println("Loading catalog from cache...")

		// Read from disk
		catalog, err = readCatalogFile(catalogCachePath)
		if err != nil {
			log.Fatalf("Failed to load catalog: %v", err)
		}
	} else { // otherwise, fetch latest
		log.Println("Fetching latest catalog...")

		// Fetch from MCP
		catalogBytes, err := fetchCatalog(platform, "fn", "4fe75bbc5a674f4f9b356b5c90567da5", "Fortnite", "Live")
		if err != nil {
			log.Fatalf("Failed to fetch catalog: %v", err)
		}

		// Parse data
		catalog, err = parseCatalog(catalogBytes)
		if err != nil {
			log.Fatalf("Failed to parse catalog: %v", err)
		}

		// Save to cache
		ioutil.WriteFile(catalogCachePath, catalogBytes, 0644)
	}

	// Sanity check catalog
	if len(catalog.Elements) != 1 || len(catalog.Elements[0].Manifests) < 1 {
		log.Fatal("Unsupported catalog")
	}

	log.Printf("Catalog %s (%s) %s loaded.\n", catalog.Elements[0].AppName, catalog.Elements[0].LabelName, catalog.Elements[0].BuildVersion)

	// Load manifest
	manifestCachePath := filepath.Join(cachePath, "manifest.json")
	if manifestID != "" { // fetch specific manifest
		log.Printf("Fetching manifest %s...", manifestID)

		var err error
		manifest, _, err = fetchManifest(fmt.Sprintf("%s/Builds/Fortnite/CloudDir/%s.manifest", defaultDownloadURL, manifestID))
		if err != nil {
			log.Fatalf("Failed to fetch manifest: %v", err)
		}
	} else if _, err := os.Stat(manifestCachePath); err == nil { // read manifest from disk
		log.Println("Loading manifest from cache...")

		manifest, err = readManifestFile(manifestCachePath)
		if err != nil {
			log.Fatalf("Failed to read manifest: %v", err)
		}
	} else { // otherwise, fetch from web
		log.Println("Fetching latest manifest...")

		var manifestBytes []byte
		manifest, manifestBytes, err = fetchManifest(catalog.GetManifestURL())
		if err != nil {
			log.Fatalf("Failed to fetch manifest: %v", err)
		}
		ioutil.WriteFile(manifestCachePath, manifestBytes, 0644)
	}

	log.Printf("Manifest %s %s loaded.\n", manifest.AppNameString, manifest.BuildVersionString)

	// Parse manifest
	manifestFiles := make(map[string]ManifestFile)
	manifestChunks := make(map[string]Chunk)
	chunkReverseMap := make(map[string]int)
	for _, file := range manifest.FileManifestList {
		// Add file
		manifestFiles[file.FileName] = file

		// Add all chunks
		for _, c := range file.FileChunkParts {
			chunkReverseMap[c.GUID]++

			if _, ok := manifestChunks[c.GUID]; !ok { // don't add duplicates
				manifestChunks[c.GUID] = NewChunk(c.GUID, manifest.ChunkHashList[c.GUID], manifest.ChunkShaList[c.GUID], manifest.DataGroupList[c.GUID], manifest.ChunkFilesizeList[c.GUID])
			}
		}
	}

	log.Printf("Found %d files and %d chunks in manifest.\n", len(manifestFiles), len(manifestChunks))

	// File filter
	if fileFilter != "" {
		tempFiles := make(map[string]ManifestFile)
		for _, fileName := range strings.Split(fileFilter, ",") {
			if f, ok := manifestFiles[fileName]; ok {
				tempFiles[fileName] = f
			}
		}
		manifestFiles = tempFiles
	}

	// Chunk cache
	chunkCache := make(map[string][]byte)

	// Download and assemble files
	for _, file := range manifestFiles {
		filePath := filepath.Join(installPath, file.FileName)

		// Check if file already exists
		if _, err := os.Stat(filePath); err == nil {
			// Open file
			diskFile, err := os.Open(filePath)
			if err == nil {
				defer diskFile.Close()

				// Calculate checksum
				hasher := sha1.New()
				_, err := io.Copy(hasher, diskFile)
				if err == nil {
					// Compare checksum
					if bytes.Equal(hasher.Sum(nil), readPackedData(file.FileHash)) {
						// Remove any trailing chunks
						for _, chunkPart := range file.FileChunkParts {
							chunkReverseMap[chunkPart.GUID]--
							if chunkReverseMap[chunkPart.GUID] < 1 {
								delete(chunkCache, chunkPart.GUID)
							}
						}

						log.Printf("File %s found on disk!\n", file.FileName)
						continue
					}
				}
			}
		}

		log.Printf("Downloading %s from %d chunks...\n", file.FileName, len(file.FileChunkParts))

		// Create outfile
		os.MkdirAll(filepath.Dir(filePath), os.ModePerm)
		outFile, err := os.Create(filePath)
		if err != nil {
			log.Printf("Failed to create %s: %v\n", filePath, err)
			continue
		}
		defer outFile.Close()

		// Write chunk data
		for _, chunkPart := range file.FileChunkParts {
			chunk := manifestChunks[chunkPart.GUID]
			chunkDataOffset := readPackedUint32(chunkPart.Offset)
			chunkDataSize := readPackedUint32(chunkPart.Size)

			var chunkReader io.ReadSeeker
			if _, ok := chunkCache[chunk.GUID]; ok {
				// Read from cache
				chunkReader = bytes.NewReader(chunkCache[chunk.GUID])
			} else {
				// Download chunk
				chunkData, err := chunk.Download(downloadURLs[rand.Intn(len(downloadURLs))])
				if err != nil {
					log.Printf("Failed to download chunk %s for file %s: %v\n", chunk.GUID, file.FileName, err)
					continue
				}

				// Create new reader
				chunkReader = bytes.NewReader(chunkData)

				// Read chunk header
				chunkHeader, err := readChunkHeader(chunkReader)
				if err != nil {
					log.Printf("Failed to read chunk header %s for file %s: %v\n", chunk.GUID, file.FileName, err)
					continue
				}

				// Decompress if needed
				if chunkHeader.StoredAs == 1 {
					// Create decompressor
					zlibReader, err := zlib.NewReader(chunkReader)
					if err != nil {
						log.Printf("Failed to create decompressor for chunk %s: %v\n", chunk.GUID, err)
						continue
					}

					// Decompress entire chunk
					chunkData, err = ioutil.ReadAll(zlibReader)
					if err != nil {
						log.Printf("Failed to decompress chunk %s: %v\n", chunk.GUID, err)
						continue
					}

					// Set reader to decompressed data
					chunkReader = bytes.NewReader(chunkData)
				} else if chunkHeader.StoredAs != 0 {
					log.Printf("Got unknown chunk (storedas: %d) %s for file %s\n", chunkHeader.StoredAs, chunk.GUID, file.FileName)
					continue
				}

				// Store in cache if needed later
				if chunkReverseMap[chunk.GUID] > 1 {
					if chunkHeader.StoredAs == 0 { // chunkData still contains header here
						chunkCache[chunk.GUID] = chunkData[62:]
					} else {
						chunkCache[chunk.GUID] = chunkData
					}
				}
			}

			// Write chunk to file
			chunkReader.Seek(int64(chunkDataOffset), io.SeekCurrent)
			_, err := io.CopyN(outFile, chunkReader, int64(chunkDataSize))
			if err != nil {
				log.Printf("Failed to write chunk %s to file %s: %v\n", chunk.GUID, file.FileName, err)
				continue
			}

			// Chunk was used once
			chunkReverseMap[chunk.GUID]--

			// Check if we still need to store chunk in cache
			if chunkReverseMap[chunk.GUID] < 1 {
				delete(chunkCache, chunk.GUID)
			}
		}
	}

	// Integrity check
	if !skipIntegrityCheck {
		log.Println("Verifying file integrity...")

		for _, file := range manifestFiles {
			filePath := filepath.Join(installPath, file.FileName)

			// Open file
			f, err := os.Open(filePath)
			if err != nil {
				log.Printf("Failed to open %s: %v\n", file.FileName, err)
				continue
			}
			defer f.Close()

			// Hash file
			hasher := sha1.New()
			if _, err := io.Copy(hasher, f); err != nil {
				log.Printf("Failed to hash %s: %v\n", file.FileName, err)
				continue
			}

			// Compare checksum
			expectedHash := readPackedData(file.FileHash)
			actualHash := hasher.Sum(nil)
			if !bytes.Equal(actualHash, expectedHash) {
				log.Printf("File %s is corrupt - got hash %s but want %s\n", file.FileName, hex.EncodeToString(actualHash), hex.EncodeToString(expectedHash))
			}
		}
	}

	log.Println("Done!")
}
