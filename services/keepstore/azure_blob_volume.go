// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.curoverse.com/arvados.git/sdk/go/arvados"
	log "github.com/Sirupsen/logrus"
	"github.com/curoverse/azure-sdk-for-go/storage"
)

const azureDefaultRequestTimeout = arvados.Duration(10 * time.Minute)

var (
	azureMaxGetBytes           int
	azureStorageAccountName    string
	azureStorageAccountKeyFile string
	azureStorageReplication    int
	azureWriteRaceInterval     = 15 * time.Second
	azureWriteRacePollTime     = time.Second
)

func readKeyFromFile(file string) (string, error) {
	buf, err := ioutil.ReadFile(file)
	if err != nil {
		return "", errors.New("reading key from " + file + ": " + err.Error())
	}
	accountKey := strings.TrimSpace(string(buf))
	if accountKey == "" {
		return "", errors.New("empty account key in " + file)
	}
	return accountKey, nil
}

type azureVolumeAdder struct {
	*Config
}

// String implements flag.Value
func (s *azureVolumeAdder) String() string {
	return "-"
}

func (s *azureVolumeAdder) Set(containerName string) error {
	s.Config.Volumes = append(s.Config.Volumes, &AzureBlobVolume{
		ContainerName:         containerName,
		StorageAccountName:    azureStorageAccountName,
		StorageAccountKeyFile: azureStorageAccountKeyFile,
		AzureReplication:      azureStorageReplication,
		ReadOnly:              deprecated.flagReadonly,
	})
	return nil
}

func init() {
	VolumeTypes = append(VolumeTypes, func() VolumeWithExamples { return &AzureBlobVolume{} })

	flag.Var(&azureVolumeAdder{theConfig},
		"azure-storage-container-volume",
		"Use the given container as a storage volume. Can be given multiple times.")
	flag.StringVar(
		&azureStorageAccountName,
		"azure-storage-account-name",
		"",
		"Azure storage account name used for subsequent --azure-storage-container-volume arguments.")
	flag.StringVar(
		&azureStorageAccountKeyFile,
		"azure-storage-account-key-file",
		"",
		"`File` containing the account key used for subsequent --azure-storage-container-volume arguments.")
	flag.IntVar(
		&azureStorageReplication,
		"azure-storage-replication",
		3,
		"Replication level to report to clients when data is stored in an Azure container.")
	flag.IntVar(
		&azureMaxGetBytes,
		"azure-max-get-bytes",
		BlockSize,
		fmt.Sprintf("Maximum bytes to request in a single GET request. If smaller than %d, use multiple concurrent range requests to retrieve a block.", BlockSize))
}

// An AzureBlobVolume stores and retrieves blocks in an Azure Blob
// container.
type AzureBlobVolume struct {
	StorageAccountName    string
	StorageAccountKeyFile string
	StorageBaseURL        string // "" means default, "core.windows.net"
	ContainerName         string
	AzureReplication      int
	ReadOnly              bool
	RequestTimeout        arvados.Duration

	azClient storage.Client
	bsClient *azureBlobClient
}

// Examples implements VolumeWithExamples.
func (*AzureBlobVolume) Examples() []Volume {
	return []Volume{
		&AzureBlobVolume{
			StorageAccountName:    "example-account-name",
			StorageAccountKeyFile: "/etc/azure_storage_account_key.txt",
			ContainerName:         "example-container-name",
			AzureReplication:      3,
			RequestTimeout:        azureDefaultRequestTimeout,
		},
		&AzureBlobVolume{
			StorageAccountName:    "cn-account-name",
			StorageAccountKeyFile: "/etc/azure_cn_storage_account_key.txt",
			StorageBaseURL:        "core.chinacloudapi.cn",
			ContainerName:         "cn-container-name",
			AzureReplication:      3,
			RequestTimeout:        azureDefaultRequestTimeout,
		},
	}
}

// Type implements Volume.
func (v *AzureBlobVolume) Type() string {
	return "Azure"
}

// Start implements Volume.
func (v *AzureBlobVolume) Start() error {
	if v.ContainerName == "" {
		return errors.New("no container name given")
	}
	if v.StorageAccountName == "" || v.StorageAccountKeyFile == "" {
		return errors.New("StorageAccountName and StorageAccountKeyFile must be given")
	}
	accountKey, err := readKeyFromFile(v.StorageAccountKeyFile)
	if err != nil {
		return err
	}
	if v.StorageBaseURL == "" {
		v.StorageBaseURL = storage.DefaultBaseURL
	}
	v.azClient, err = storage.NewClient(v.StorageAccountName, accountKey, v.StorageBaseURL, storage.DefaultAPIVersion, true)
	if err != nil {
		return fmt.Errorf("creating Azure storage client: %s", err)
	}

	if v.RequestTimeout == 0 {
		v.RequestTimeout = azureDefaultRequestTimeout
	}
	v.azClient.HTTPClient = &http.Client{
		Timeout: time.Duration(v.RequestTimeout),
	}
	bs := v.azClient.GetBlobService()
	v.bsClient = &azureBlobClient{
		client: &bs,
	}

	ok, err := v.bsClient.ContainerExists(v.ContainerName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Azure container %q does not exist", v.ContainerName)
	}
	return nil
}

// DeviceID returns a globally unique ID for the storage container.
func (v *AzureBlobVolume) DeviceID() string {
	return "azure://" + v.StorageBaseURL + "/" + v.StorageAccountName + "/" + v.ContainerName
}

// Return true if expires_at metadata attribute is found on the block
func (v *AzureBlobVolume) checkTrashed(loc string) (bool, map[string]string, error) {
	metadata, err := v.bsClient.GetBlobMetadata(v.ContainerName, loc)
	if err != nil {
		return false, metadata, v.translateError(err)
	}
	if metadata["expires_at"] != "" {
		return true, metadata, nil
	}
	return false, metadata, nil
}

// Get reads a Keep block that has been stored as a block blob in the
// container.
//
// If the block is younger than azureWriteRaceInterval and is
// unexpectedly empty, assume a PutBlob operation is in progress, and
// wait for it to finish writing.
func (v *AzureBlobVolume) Get(ctx context.Context, loc string, buf []byte) (int, error) {
	trashed, _, err := v.checkTrashed(loc)
	if err != nil {
		return 0, err
	}
	if trashed {
		return 0, os.ErrNotExist
	}
	var deadline time.Time
	haveDeadline := false
	size, err := v.get(ctx, loc, buf)
	for err == nil && size == 0 && loc != "d41d8cd98f00b204e9800998ecf8427e" {
		// Seeing a brand new empty block probably means we're
		// in a race with CreateBlob, which under the hood
		// (apparently) does "CreateEmpty" and "CommitData"
		// with no additional transaction locking.
		if !haveDeadline {
			t, err := v.Mtime(loc)
			if err != nil {
				log.Print("Got empty block (possible race) but Mtime failed: ", err)
				break
			}
			deadline = t.Add(azureWriteRaceInterval)
			if time.Now().After(deadline) {
				break
			}
			log.Printf("Race? Block %s is 0 bytes, %s old. Polling until %s", loc, time.Since(t), deadline)
			haveDeadline = true
		} else if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(azureWriteRacePollTime):
		}
		size, err = v.get(ctx, loc, buf)
	}
	if haveDeadline {
		log.Printf("Race ended with size==%d", size)
	}
	return size, err
}

func (v *AzureBlobVolume) get(ctx context.Context, loc string, buf []byte) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	expectSize := len(buf)
	if azureMaxGetBytes < BlockSize {
		// Unfortunately the handler doesn't tell us how long the blob
		// is expected to be, so we have to ask Azure.
		props, err := v.bsClient.GetBlobProperties(v.ContainerName, loc)
		if err != nil {
			return 0, v.translateError(err)
		}
		if props.ContentLength > int64(BlockSize) || props.ContentLength < 0 {
			return 0, fmt.Errorf("block %s invalid size %d (max %d)", loc, props.ContentLength, BlockSize)
		}
		expectSize = int(props.ContentLength)
	}

	if expectSize == 0 {
		return 0, nil
	}

	// We'll update this actualSize if/when we get the last piece.
	actualSize := -1
	pieces := (expectSize + azureMaxGetBytes - 1) / azureMaxGetBytes
	errors := make(chan error, pieces)
	var wg sync.WaitGroup
	wg.Add(pieces)
	for p := 0; p < pieces; p++ {
		// Each goroutine retrieves one piece. If we hit an
		// error, it is sent to the errors chan so get() can
		// return it -- but only if the error happens before
		// ctx is done. This way, if ctx is done before we hit
		// any other error (e.g., requesting client has hung
		// up), we return the original ctx.Err() instead of
		// the secondary errors from the transfers that got
		// interrupted as a result.
		go func(p int) {
			defer wg.Done()
			startPos := p * azureMaxGetBytes
			endPos := startPos + azureMaxGetBytes
			if endPos > expectSize {
				endPos = expectSize
			}
			var rdr io.ReadCloser
			var err error
			gotRdr := make(chan struct{})
			go func() {
				defer close(gotRdr)
				if startPos == 0 && endPos == expectSize {
					rdr, err = v.bsClient.GetBlob(v.ContainerName, loc)
				} else {
					rdr, err = v.bsClient.GetBlobRange(v.ContainerName, loc, fmt.Sprintf("%d-%d", startPos, endPos-1), nil)
				}
			}()
			select {
			case <-ctx.Done():
				go func() {
					<-gotRdr
					if err == nil {
						rdr.Close()
					}
				}()
				return
			case <-gotRdr:
			}
			if err != nil {
				errors <- err
				cancel()
				return
			}
			go func() {
				// Close the reader when the client
				// hangs up or another piece fails
				// (possibly interrupting ReadFull())
				// or when all pieces succeed and
				// get() returns.
				<-ctx.Done()
				rdr.Close()
			}()
			n, err := io.ReadFull(rdr, buf[startPos:endPos])
			if pieces == 1 && (err == io.ErrUnexpectedEOF || err == io.EOF) {
				// If we don't know the actual size,
				// and just tried reading 64 MiB, it's
				// normal to encounter EOF.
			} else if err != nil {
				if ctx.Err() == nil {
					errors <- err
				}
				cancel()
				return
			}
			if p == pieces-1 {
				actualSize = startPos + n
			}
		}(p)
	}
	wg.Wait()
	close(errors)
	if len(errors) > 0 {
		return 0, v.translateError(<-errors)
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return actualSize, nil
}

// Compare the given data with existing stored data.
func (v *AzureBlobVolume) Compare(ctx context.Context, loc string, expect []byte) error {
	trashed, _, err := v.checkTrashed(loc)
	if err != nil {
		return err
	}
	if trashed {
		return os.ErrNotExist
	}
	var rdr io.ReadCloser
	gotRdr := make(chan struct{})
	go func() {
		defer close(gotRdr)
		rdr, err = v.bsClient.GetBlob(v.ContainerName, loc)
	}()
	select {
	case <-ctx.Done():
		go func() {
			<-gotRdr
			if err == nil {
				rdr.Close()
			}
		}()
		return ctx.Err()
	case <-gotRdr:
	}
	if err != nil {
		return v.translateError(err)
	}
	defer rdr.Close()
	return compareReaderWithBuf(ctx, rdr, expect, loc[:32])
}

// Put stores a Keep block as a block blob in the container.
func (v *AzureBlobVolume) Put(ctx context.Context, loc string, block []byte) error {
	if v.ReadOnly {
		return MethodDisabledError
	}
	// Send the block data through a pipe, so that (if we need to)
	// we can close the pipe early and abandon our
	// CreateBlockBlobFromReader() goroutine, without worrying
	// about CreateBlockBlobFromReader() accessing our block
	// buffer after we release it.
	bufr, bufw := io.Pipe()
	go func() {
		io.Copy(bufw, bytes.NewReader(block))
		bufw.Close()
	}()
	errChan := make(chan error)
	go func() {
		var body io.Reader = bufr
		if len(block) == 0 {
			// We must send a "Content-Length: 0" header,
			// but the http client interprets
			// ContentLength==0 as "unknown" unless it can
			// confirm by introspection that Body will
			// read 0 bytes.
			body = http.NoBody
			bufr.Close()
		}
		errChan <- v.bsClient.CreateBlockBlobFromReader(v.ContainerName, loc, uint64(len(block)), body, nil)
	}()
	select {
	case <-ctx.Done():
		theConfig.debugLogf("%s: taking CreateBlockBlobFromReader's input away: %s", v, ctx.Err())
		// Our pipe might be stuck in Write(), waiting for
		// io.Copy() to read. If so, un-stick it. This means
		// CreateBlockBlobFromReader will get corrupt data,
		// but that's OK: the size won't match, so the write
		// will fail.
		go io.Copy(ioutil.Discard, bufr)
		// CloseWithError() will return once pending I/O is done.
		bufw.CloseWithError(ctx.Err())
		theConfig.debugLogf("%s: abandoning CreateBlockBlobFromReader goroutine", v)
		return ctx.Err()
	case err := <-errChan:
		return err
	}
}

// Touch updates the last-modified property of a block blob.
func (v *AzureBlobVolume) Touch(loc string) error {
	if v.ReadOnly {
		return MethodDisabledError
	}
	trashed, metadata, err := v.checkTrashed(loc)
	if err != nil {
		return err
	}
	if trashed {
		return os.ErrNotExist
	}

	metadata["touch"] = fmt.Sprintf("%d", time.Now())
	return v.bsClient.SetBlobMetadata(v.ContainerName, loc, metadata, nil)
}

// Mtime returns the last-modified property of a block blob.
func (v *AzureBlobVolume) Mtime(loc string) (time.Time, error) {
	trashed, _, err := v.checkTrashed(loc)
	if err != nil {
		return time.Time{}, err
	}
	if trashed {
		return time.Time{}, os.ErrNotExist
	}

	props, err := v.bsClient.GetBlobProperties(v.ContainerName, loc)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC1123, props.LastModified)
}

// IndexTo writes a list of Keep blocks that are stored in the
// container.
func (v *AzureBlobVolume) IndexTo(prefix string, writer io.Writer) error {
	params := storage.ListBlobsParameters{
		Prefix:  prefix,
		Include: "metadata",
	}
	for {
		resp, err := v.bsClient.ListBlobs(v.ContainerName, params)
		if err != nil {
			return err
		}
		for _, b := range resp.Blobs {
			t, err := time.Parse(time.RFC1123, b.Properties.LastModified)
			if err != nil {
				return err
			}
			if !v.isKeepBlock(b.Name) {
				continue
			}
			if b.Properties.ContentLength == 0 && t.Add(azureWriteRaceInterval).After(time.Now()) {
				// A new zero-length blob is probably
				// just a new non-empty blob that
				// hasn't committed its data yet (see
				// Get()), and in any case has no
				// value.
				continue
			}
			if b.Metadata["expires_at"] != "" {
				// Trashed blob; exclude it from response
				continue
			}
			fmt.Fprintf(writer, "%s+%d %d\n", b.Name, b.Properties.ContentLength, t.UnixNano())
		}
		if resp.NextMarker == "" {
			return nil
		}
		params.Marker = resp.NextMarker
	}
}

// Trash a Keep block.
func (v *AzureBlobVolume) Trash(loc string) error {
	if v.ReadOnly {
		return MethodDisabledError
	}

	// Ideally we would use If-Unmodified-Since, but that
	// particular condition seems to be ignored by Azure. Instead,
	// we get the Etag before checking Mtime, and use If-Match to
	// ensure we don't delete data if Put() or Touch() happens
	// between our calls to Mtime() and DeleteBlob().
	props, err := v.bsClient.GetBlobProperties(v.ContainerName, loc)
	if err != nil {
		return err
	}
	if t, err := v.Mtime(loc); err != nil {
		return err
	} else if time.Since(t) < theConfig.BlobSignatureTTL.Duration() {
		return nil
	}

	// If TrashLifetime == 0, just delete it
	if theConfig.TrashLifetime == 0 {
		return v.bsClient.DeleteBlob(v.ContainerName, loc, map[string]string{
			"If-Match": props.Etag,
		})
	}

	// Otherwise, mark as trash
	return v.bsClient.SetBlobMetadata(v.ContainerName, loc, map[string]string{
		"expires_at": fmt.Sprintf("%d", time.Now().Add(theConfig.TrashLifetime.Duration()).Unix()),
	}, map[string]string{
		"If-Match": props.Etag,
	})
}

// Untrash a Keep block.
// Delete the expires_at metadata attribute
func (v *AzureBlobVolume) Untrash(loc string) error {
	// if expires_at does not exist, return NotFoundError
	metadata, err := v.bsClient.GetBlobMetadata(v.ContainerName, loc)
	if err != nil {
		return v.translateError(err)
	}
	if metadata["expires_at"] == "" {
		return os.ErrNotExist
	}

	// reset expires_at metadata attribute
	metadata["expires_at"] = ""
	err = v.bsClient.SetBlobMetadata(v.ContainerName, loc, metadata, nil)
	return v.translateError(err)
}

// Status returns a VolumeStatus struct with placeholder data.
func (v *AzureBlobVolume) Status() *VolumeStatus {
	return &VolumeStatus{
		DeviceNum: 1,
		BytesFree: BlockSize * 1000,
		BytesUsed: 1,
	}
}

// String returns a volume label, including the container name.
func (v *AzureBlobVolume) String() string {
	return fmt.Sprintf("azure-storage-container:%+q", v.ContainerName)
}

// Writable returns true, unless the -readonly flag was on when the
// volume was added.
func (v *AzureBlobVolume) Writable() bool {
	return !v.ReadOnly
}

// Replication returns the replication level of the container, as
// specified by the -azure-storage-replication argument.
func (v *AzureBlobVolume) Replication() int {
	return v.AzureReplication
}

// If possible, translate an Azure SDK error to a recognizable error
// like os.ErrNotExist.
func (v *AzureBlobVolume) translateError(err error) error {
	switch {
	case err == nil:
		return err
	case strings.Contains(err.Error(), "Not Found"):
		// "storage: service returned without a response body (404 Not Found)"
		return os.ErrNotExist
	default:
		return err
	}
}

var keepBlockRegexp = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (v *AzureBlobVolume) isKeepBlock(s string) bool {
	return keepBlockRegexp.MatchString(s)
}

// EmptyTrash looks for trashed blocks that exceeded TrashLifetime
// and deletes them from the volume.
func (v *AzureBlobVolume) EmptyTrash() {
	var bytesDeleted, bytesInTrash int64
	var blocksDeleted, blocksInTrash int
	params := storage.ListBlobsParameters{Include: "metadata"}

	for {
		resp, err := v.bsClient.ListBlobs(v.ContainerName, params)
		if err != nil {
			log.Printf("EmptyTrash: ListBlobs: %v", err)
			break
		}
		for _, b := range resp.Blobs {
			// Check if the block is expired
			if b.Metadata["expires_at"] == "" {
				continue
			}

			blocksInTrash++
			bytesInTrash += b.Properties.ContentLength

			expiresAt, err := strconv.ParseInt(b.Metadata["expires_at"], 10, 64)
			if err != nil {
				log.Printf("EmptyTrash: ParseInt(%v): %v", b.Metadata["expires_at"], err)
				continue
			}

			if expiresAt > time.Now().Unix() {
				continue
			}

			err = v.bsClient.DeleteBlob(v.ContainerName, b.Name, map[string]string{
				"If-Match": b.Properties.Etag,
			})
			if err != nil {
				log.Printf("EmptyTrash: DeleteBlob(%v): %v", b.Name, err)
				continue
			}
			blocksDeleted++
			bytesDeleted += b.Properties.ContentLength
		}
		if resp.NextMarker == "" {
			break
		}
		params.Marker = resp.NextMarker
	}

	log.Printf("EmptyTrash stats for %v: Deleted %v bytes in %v blocks. Remaining in trash: %v bytes in %v blocks.", v.String(), bytesDeleted, blocksDeleted, bytesInTrash-bytesDeleted, blocksInTrash-blocksDeleted)
}

// InternalStats returns bucket I/O and API call counters.
func (v *AzureBlobVolume) InternalStats() interface{} {
	return &v.bsClient.stats
}

type azureBlobStats struct {
	statsTicker
	Ops              uint64
	GetOps           uint64
	GetRangeOps      uint64
	GetMetadataOps   uint64
	GetPropertiesOps uint64
	CreateOps        uint64
	SetMetadataOps   uint64
	DelOps           uint64
	ListOps          uint64
}

func (s *azureBlobStats) TickErr(err error) {
	if err == nil {
		return
	}
	errType := fmt.Sprintf("%T", err)
	if err, ok := err.(storage.AzureStorageServiceError); ok {
		errType = errType + fmt.Sprintf(" %d (%s)", err.StatusCode, err.Code)
	}
	log.Printf("errType %T, err %s", err, err)
	s.statsTicker.TickErr(err, errType)
}

// azureBlobClient wraps storage.BlobStorageClient in order to count
// I/O and API usage stats.
type azureBlobClient struct {
	client *storage.BlobStorageClient
	stats  azureBlobStats
}

func (c *azureBlobClient) ContainerExists(cname string) (bool, error) {
	c.stats.Tick(&c.stats.Ops)
	ok, err := c.client.ContainerExists(cname)
	c.stats.TickErr(err)
	return ok, err
}

func (c *azureBlobClient) GetBlobMetadata(cname, bname string) (map[string]string, error) {
	c.stats.Tick(&c.stats.Ops, &c.stats.GetMetadataOps)
	m, err := c.client.GetBlobMetadata(cname, bname)
	c.stats.TickErr(err)
	return m, err
}

func (c *azureBlobClient) GetBlobProperties(cname, bname string) (*storage.BlobProperties, error) {
	c.stats.Tick(&c.stats.Ops, &c.stats.GetPropertiesOps)
	p, err := c.client.GetBlobProperties(cname, bname)
	c.stats.TickErr(err)
	return p, err
}

func (c *azureBlobClient) GetBlob(cname, bname string) (io.ReadCloser, error) {
	c.stats.Tick(&c.stats.Ops, &c.stats.GetOps)
	rdr, err := c.client.GetBlob(cname, bname)
	c.stats.TickErr(err)
	return NewCountingReader(rdr, c.stats.TickInBytes), err
}

func (c *azureBlobClient) GetBlobRange(cname, bname, byterange string, hdrs map[string]string) (io.ReadCloser, error) {
	c.stats.Tick(&c.stats.Ops, &c.stats.GetRangeOps)
	rdr, err := c.client.GetBlobRange(cname, bname, byterange, hdrs)
	c.stats.TickErr(err)
	return NewCountingReader(rdr, c.stats.TickInBytes), err
}

func (c *azureBlobClient) CreateBlockBlobFromReader(cname, bname string, size uint64, rdr io.Reader, hdrs map[string]string) error {
	c.stats.Tick(&c.stats.Ops, &c.stats.CreateOps)
	if size != 0 {
		rdr = NewCountingReader(rdr, c.stats.TickOutBytes)
	}
	err := c.client.CreateBlockBlobFromReader(cname, bname, size, rdr, hdrs)
	c.stats.TickErr(err)
	return err
}

func (c *azureBlobClient) SetBlobMetadata(cname, bname string, m, hdrs map[string]string) error {
	c.stats.Tick(&c.stats.Ops, &c.stats.SetMetadataOps)
	err := c.client.SetBlobMetadata(cname, bname, m, hdrs)
	c.stats.TickErr(err)
	return err
}

func (c *azureBlobClient) ListBlobs(cname string, params storage.ListBlobsParameters) (storage.BlobListResponse, error) {
	c.stats.Tick(&c.stats.Ops, &c.stats.ListOps)
	resp, err := c.client.ListBlobs(cname, params)
	c.stats.TickErr(err)
	return resp, err
}

func (c *azureBlobClient) DeleteBlob(cname, bname string, hdrs map[string]string) error {
	c.stats.Tick(&c.stats.Ops, &c.stats.DelOps)
	err := c.client.DeleteBlob(cname, bname, hdrs)
	c.stats.TickErr(err)
	return err
}
