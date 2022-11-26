/*
 * MinIO Object Storage (c) 2021 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package web3

import (
	"context"
	"github.com/minio/minio-go/v7/pkg/tags"
	"github.com/minio/minio/internal/db"
	"github.com/minio/pkg/bucket/policy"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/minio/cli"
	"github.com/minio/madmin-go"
	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/internal/config"
	"github.com/minio/minio/internal/logger"
	"github.com/minio/pkg/env"
)

func init() {
	const s3GatewayTemplate = `NAME:
  {{.HelpName}} - {{.Usage}}

USAGE:
  {{.HelpName}} {{if .VisibleFlags}}[FLAGS]{{end}} [ENDPOINT]
{{if .VisibleFlags}}
FLAGS:
  {{range .VisibleFlags}}{{.}}
  {{end}}{{end}}
ENDPOINT:
  s3 server endpoint. Default ENDPOINT is https://127.0.0.1:5001

EXAMPLES:ß
  1. Start minio gateway server for AWS Web3 backend with edge caching enabled
     {{.Prompt}} {{.EnvVarSetCommand}} MINIO_ROOT_USER{{.AssignmentOperator}}accesskey
     {{.Prompt}} {{.EnvVarSetCommand}} MINIO_ROOT_PASSWORD{{.AssignmentOperator}}secretkey
     {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_DRIVES{{.AssignmentOperator}}"/mnt/drive1,/mnt/drive2,/mnt/drive3,/mnt/drive4"
     {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_EXCLUDE{{.AssignmentOperator}}"bucket1/*,*.png"
     {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_QUOTA{{.AssignmentOperator}}90
     {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_AFTER{{.AssignmentOperator}}3
     {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_WATERMARK_LOW{{.AssignmentOperator}}75
     {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_WATERMARK_HIGH{{.AssignmentOperator}}85
     {{.Prompt}} {{.HelpName}}
`

	minio.RegisterGatewayCommand(cli.Command{
		Name:               minio.Web3BackendGateway,
		Usage:              "ipfs Storage Service (web3ß)",
		Action:             web3GatewayMain,
		CustomHelpTemplate: s3GatewayTemplate,
		HideHelpCommand:    true,
	})
}

// Handler for 'minio gateway web3' command line.
func web3GatewayMain(ctx *cli.Context) {
	args := ctx.Args()
	if !ctx.Args().Present() {
		args = cli.Args{"https://s3.amazonaws.com"}
	}

	serverAddr := ctx.GlobalString("address")
	if serverAddr == "" || serverAddr == ":"+minio.GlobalMinioDefaultPort {
		serverAddr = ctx.String("address")
	}
	// Validate gateway arguments.
	logger.FatalIf(minio.ValidateGatewayArguments(serverAddr, args.First()), "Invalid argument")

	// Start the gateway..
	minio.StartGateway(ctx, &Web3{
		host:  args.First(),
		debug: env.Get("_MINIO_SERVER_DEBUG", config.EnableOff) == config.EnableOn,
	})
}

// Web3 implements Gateway.
type Web3 struct {
	host  string
	debug bool
}

// Name implements Gateway interface.
func (g *Web3) Name() string {
	return minio.Web3BackendGateway
}

const letterBytes = "abcdefghijklmnopqrstuvwxyz01234569"
const (
	letterIdxBits = 6                    // 6 bits to represent a letter index
	letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
)

// randString generates random names and prepends them with a known prefix.
func randString(n int, src rand.Source, prefix string) string {
	b := make([]byte, n)
	// A rand.Int63() generates 63 random bits, enough for letterIdxMax letters!
	for i, cache, remain := n-1, src.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = src.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}
	return prefix + string(b[0:30-len(prefix)])
}

// Chains all credential types, in the following order:
//   - AWS env vars (i.e. AWS_ACCESS_KEY_ID)
//   - AWS creds file (i.e. AWS_SHARED_CREDENTIALS_FILE or ~/.aws/credentials)
//   - Static credentials provided by user (i.e. MINIO_ROOT_USER/MINIO_ACCESS_KEY)
var defaultProviders = []credentials.Provider{
	&credentials.EnvAWS{},
	&credentials.FileAWSCredentials{},
}

// Chains all credential types, in the following order:
//   - AWS env vars (i.e. AWS_ACCESS_KEY_ID)
//   - AWS creds file (i.e. AWS_SHARED_CREDENTIALS_FILE or ~/.aws/credentials)
//   - IAM profile based credentials. (performs an HTTP
//     call to a pre-defined endpoint, only valid inside
//     configured ec2 instances)
//   - Static credentials provided by user (i.e. MINIO_ROOT_USER/MINIO_ACCESS_KEY)
var defaultAWSCredProviders = []credentials.Provider{
	&credentials.EnvAWS{},
	&credentials.FileAWSCredentials{},
	&credentials.IAM{
		// you can specify a custom STS endpoint.
		Endpoint: env.Get("MINIO_GATEWAY_S3_STS_ENDPOINT", ""),
		Client: &http.Client{
			Transport: minio.NewGatewayHTTPTransport(),
		},
	},
}

// new - Initializes a new client by auto probing Web3 server signature.
func (g *Web3) new(creds madmin.Credentials, transport http.RoundTripper) (*miniogo.Core, error) {
	urlStr := g.host
	if urlStr == "" {
		urlStr = "https://127.0.0.1:5001"
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}

	// Override default params if the host is provided
	endpoint, secure, err := minio.ParseGatewayEndpoint(urlStr)
	if err != nil {
		return nil, err
	}

	var chainCreds *credentials.Credentials
	//if s3utils.IsAmazonEndpoint(*u) {
	//	// If we see an Amazon Web3 endpoint, then we use more ways to fetch backend credentials.
	//	// Specifically IAM style rotating credentials are only supported with AWS Web3 endpoint.
	//	chainCreds = NewChainCredentials(defaultAWSCredProviders)
	//} else {
	//	chainCreds = NewChainCredentials(defaultProviders)
	//}

	optionsStaticCreds := &miniogo.Options{
		Creds:        credentials.NewStaticV4(creds.AccessKey, creds.SecretKey, creds.SessionToken),
		Secure:       secure,
		Region:       s3utils.GetRegionFromURL(*u),
		BucketLookup: miniogo.BucketLookupAuto,
		Transport:    transport,
	}

	optionsChainCreds := &miniogo.Options{
		Creds:        chainCreds,
		Secure:       secure,
		Region:       s3utils.GetRegionFromURL(*u),
		BucketLookup: miniogo.BucketLookupAuto,
		Transport:    transport,
	}

	clntChain, err := miniogo.New(endpoint, optionsChainCreds)
	if err != nil {
		return nil, err
	}

	clntStatic, err := miniogo.New(endpoint, optionsStaticCreds)
	if err != nil {
		return nil, err
	}

	if g.debug {
		clntChain.TraceOn(os.Stderr)
		clntStatic.TraceOn(os.Stderr)
	}

	probeBucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "probe-bucket-sign-")

	if _, err = clntStatic.BucketExists(context.Background(), probeBucketName); err != nil {
		switch miniogo.ToErrorResponse(err).Code {
		case "InvalidAccessKeyId":
			// Check if the provided keys are valid for chain.
			if _, err = clntChain.BucketExists(context.Background(), probeBucketName); err != nil {
				if miniogo.ToErrorResponse(err).Code != "AccessDenied" {
					return nil, err
				}
			}
			return &miniogo.Core{Client: clntChain}, nil
		case "AccessDenied":
			// this is a good error means backend is reachable
			// and credentials are valid but credentials don't
			// have access to 'probeBucketName' which is harmless.
			return &miniogo.Core{Client: clntStatic}, nil
		default:
			return nil, err
		}
	}

	// if static keys are valid always use static keys.
	return &miniogo.Core{Client: clntStatic}, nil
}

// NewGatewayLayer returns s3 ObjectLayer.
func (g *Web3) NewGatewayLayer(creds madmin.Credentials) (minio.ObjectLayer, error) {
	s := objects{
		db: db.NewObjectDB(),
	}
	return &s, nil
}

// objects implements gateway for MinIO and S3 compatible object storage servers.
type objects struct {
	db *db.ObjectDB
	minio.ObjectLayer
}

func (o *objects) SetDriveCounts() []int {
	return nil
}
func (o *objects) NewNSLock(bucket string, objects ...string) minio.RWLocker {
	//TODO implement me
	panic("implement me")
}
func (o *objects) BackendInfo() madmin.BackendInfo {
	//TODO implement me
	return madmin.BackendInfo{}
}

func (o *objects) StorageInfo(ctx context.Context) (minio.StorageInfo, []error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) LocalStorageInfo(ctx context.Context) (minio.StorageInfo, []error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) MakeBucketWithLocation(ctx context.Context, bucket string, opts minio.MakeBucketOptions) error {
	if opts.LockEnabled || opts.VersioningEnabled {
		return minio.NotImplemented{}
	}
	if s3utils.CheckValidBucketName(bucket) != nil {
		return minio.BucketNameInvalid{Bucket: bucket}
	}
	err := o.db.MakeBucket(ctx, bucket, miniogo.MakeBucketOptions{Region: opts.Location})
	if err != nil {
		return minio.ErrorRespToObjectError(err, bucket)
	}
	return err
}

func (o *objects) GetBucketInfo(ctx context.Context, bucket string, opts minio.BucketOptions) (bucketInfo minio.BucketInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) ListBuckets(ctx context.Context, opts minio.BucketOptions) (buckets []minio.BucketInfo, err error) {
	return o.db.ListBuckets(ctx, opts)
}

func (o *objects) DeleteBucket(ctx context.Context, bucket string, opts minio.DeleteBucketOptions) error {
	//TODO implement me
	panic("implement me")
}

func (o *objects) ListObjects(ctx context.Context, bucket, prefix, marker, delimiter string, maxKeys int) (result minio.ListObjectsInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) ListObjectsV2(ctx context.Context, bucket, prefix, continuationToken, delimiter string, maxKeys int, fetchOwner bool, startAfter string) (result minio.ListObjectsV2Info, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) ListObjectVersions(ctx context.Context, bucket, prefix, marker, versionMarker, delimiter string, maxKeys int) (result minio.ListObjectVersionsInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) Walk(ctx context.Context, bucket, prefix string, results chan<- minio.ObjectInfo, opts minio.ObjectOptions) error {
	//TODO implement me
	panic("implement me")
}

func (o *objects) GetObjectNInfo(ctx context.Context, bucket, object string, rs *minio.HTTPRangeSpec, h http.Header, lockType minio.LockType, opts minio.ObjectOptions) (reader *minio.GetObjectReader, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) GetObjectInfo(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) PutObject(ctx context.Context, bucket, object string, data *minio.PutObjReader, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) CopyObject(ctx context.Context, srcBucket, srcObject, destBucket, destObject string, srcInfo minio.ObjectInfo, srcOpts, dstOpts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) DeleteObject(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (minio.ObjectInfo, error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) DeleteObjects(ctx context.Context, bucket string, objects []minio.ObjectToDelete, opts minio.ObjectOptions) ([]minio.DeletedObject, []error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) TransitionObject(ctx context.Context, bucket, object string, opts minio.ObjectOptions) error {
	//TODO implement me
	panic("implement me")
}

func (o *objects) RestoreTransitionedObject(ctx context.Context, bucket, object string, opts minio.ObjectOptions) error {
	//TODO implement me
	panic("implement me")
}

func (o *objects) ListMultipartUploads(ctx context.Context, bucket, prefix, keyMarker, uploadIDMarker, delimiter string, maxUploads int) (result minio.ListMultipartsInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) NewMultipartUpload(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (result *minio.NewMultipartUploadResult, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) CopyObjectPart(ctx context.Context, srcBucket, srcObject, destBucket, destObject string, uploadID string, partID int, startOffset int64, length int64, srcInfo minio.ObjectInfo, srcOpts, dstOpts minio.ObjectOptions) (info minio.PartInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, data *minio.PutObjReader, opts minio.ObjectOptions) (info minio.PartInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) ListObjectParts(ctx context.Context, bucket, object, uploadID string, partNumberMarker int, maxParts int, opts minio.ObjectOptions) (result minio.ListPartsInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) AbortMultipartUpload(ctx context.Context, bucket, object, uploadID string, opts minio.ObjectOptions) error {
	//TODO implement me
	panic("implement me")
}

func (o *objects) CompleteMultipartUpload(ctx context.Context, bucket, object, uploadID string, uploadedParts []minio.CompletePart, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) SetBucketPolicy(ctx context.Context, s string, policy *policy.Policy) error {
	//TODO implement me
	panic("implement me")
}

func (o *objects) GetBucketPolicy(ctx context.Context, s string) (*policy.Policy, error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) DeleteBucketPolicy(ctx context.Context, s string) error {
	//TODO implement me
	panic("implement me")
}

func (o *objects) IsNotificationSupported() bool {
	//TODO implement me
	panic("implement me")
}

func (o *objects) IsListenSupported() bool {
	//TODO implement me
	panic("implement me")
}

func (o *objects) HealFormat(ctx context.Context, dryRun bool) (madmin.HealResultItem, error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) HealBucket(ctx context.Context, bucket string, opts madmin.HealOpts) (madmin.HealResultItem, error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) HealObject(ctx context.Context, bucket, object, versionID string, opts madmin.HealOpts) (madmin.HealResultItem, error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) HealObjects(ctx context.Context, bucket, prefix string, opts madmin.HealOpts, fn minio.HealObjectFn) error {
	//TODO implement me
	panic("implement me")
}

func (o *objects) GetMetrics(ctx context.Context) (*minio.BackendMetrics, error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) Health(ctx context.Context, opts minio.HealthOptions) minio.HealthResult {
	//TODO implement me
	panic("implement me")
}

func (o *objects) ReadHealth(ctx context.Context) bool {
	//TODO implement me
	panic("implement me")
}

func (o *objects) PutObjectMetadata(ctx context.Context, s string, s2 string, options minio.ObjectOptions) (minio.ObjectInfo, error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) PutObjectTags(ctx context.Context, s string, s2 string, s3 string, options minio.ObjectOptions) (minio.ObjectInfo, error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) GetObjectTags(ctx context.Context, s string, s2 string, options minio.ObjectOptions) (*tags.Tags, error) {
	//TODO implement me
	panic("implement me")
}

func (o *objects) DeleteObjectTags(ctx context.Context, s string, s2 string, options minio.ObjectOptions) (minio.ObjectInfo, error) {
	//TODO implement me
	panic("implement me")
}

// Shutdown saves any gateway metadata to disk
// if necessary and reload upon next restart.
func (o *objects) Shutdown(ctx context.Context) error {
	return nil
}

// GetMultipartInfo returns multipart info of the uploadId of the object
func (o *objects) GetMultipartInfo(ctx context.Context, bucket, object, uploadID string, opts minio.ObjectOptions) (result minio.MultipartInfo, err error) {
	result.Bucket = bucket
	result.Object = object
	result.UploadID = uploadID
	return result, nil
}

// IsCompressionSupported returns whether compression is applicable for this layer.
func (o *objects) IsCompressionSupported() bool {
	return false
}

// IsEncryptionSupported returns whether server side encryption is implemented for this layer.
func (o *objects) IsEncryptionSupported() bool {
	return minio.GlobalKMS != nil || minio.GlobalGatewaySSE.IsSet()
}

func (o *objects) IsTaggingSupported() bool {
	return true
}
