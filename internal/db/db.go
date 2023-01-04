package db

import (
	"context"
	"fmt"
	minioClient "github.com/minio/minio-go/v7"
	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/internal/db/mysql/model"
	"github.com/minio/minio/internal/db/mysql/query"
	"github.com/minio/pkg/env"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"strings"
	"time"
)

func connectDB(dsn string) (db *gorm.DB) {
	var err error
	if strings.HasSuffix(dsn, "sqlite.db") {
		db, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	} else {
		db, err = gorm.Open(mysql.Open(dsn))
	}
	if err != nil {
		panic(fmt.Errorf("connect db fail: %w", err))
	}
	return db
}

type ObjectDB struct {
	db *gorm.DB
}

func NewObjectDB() *ObjectDB {
	dbType := env.Get("DB_TYPE", "mysql")
	objectDB := ObjectDB{}
	switch strings.ToLower(dbType) {
	case "mysql", "sqlite":
		//root:123456@tcp(localhost:3306)/tizi365?charset=utf8&parseTime=True&loc=Local&timeout=10s&readTimeout=30s&writeTimeout=60s
		//username   数据库账号
		//password   数据库密码
		//host       数据库连接地址，可以是Ip或者域名
		//port       数据库端口
		//Dbname     数据库名
		objectDB.db = connectDB("root:mysql123456@tcp(127.0.0.1:3306)/oss?charset=utf8&parseTime=True&loc=Local&timeout=10s&readTimeout=30s&writeTimeout=60s")
	default:
		panic("")
	}
	return &objectDB
}

func (db *ObjectDB) ListBuckets(ctx context.Context, opts minio.BucketOptions) (buckets []minio.BucketInfo, err error) {
	b := query.Use(db.db).TNsBucket
	bs, err := b.WithContext(ctx).Find()
	if err != nil {
		return nil, err
	}
	length := len(bs)
	buckets = make([]minio.BucketInfo, 0, length)
	for i := 0; i < len(bs); i++ {
		buckets = append(buckets, minio.BucketInfo{Name: bs[i].Name, Created: bs[i].CreatedAt})
	}
	return buckets, nil
}

func (db *ObjectDB) MakeBucket(ctx context.Context, bucketName string, options minioClient.MakeBucketOptions) error {
	bucket := &model.TNsBucket{Name: bucketName, CreatedAt: time.Now(), Location: options.Region}
	b := query.Use(db.db).TNsBucket
	return b.WithContext(ctx).Create(bucket)
}

func (db *ObjectDB) GetBucketInfo(ctx context.Context, bucket string, opts minio.BucketOptions) (bucketInfo minio.BucketInfo, err error) {
	b := query.Use(db.db).TNsBucket
	first, err := b.WithContext(ctx).Where(b.Name.Eq(bucket)).First()
	if err != nil {
		return minio.BucketInfo{}, err
	}
	bucketInfo.Name = first.Name
	bucketInfo.Created = first.CreatedAt
	return
}
