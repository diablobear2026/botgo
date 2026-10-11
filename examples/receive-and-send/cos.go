package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/tencentyun/cos-go-sdk-v5"
)

// presignedURLExpiry 预签名URL的有效期，QQ服务器需要在这个时间窗口内把文件拉走
const presignedURLExpiry = 10 * time.Minute

// buildBucketURL 拼出 COS bucket 的访问域名
func buildBucketURL(bucket, region string) (*url.URL, error) {
	if bucket == "" || region == "" {
		return nil, errors.New("COS bucket/region 不能为空")
	}
	return url.Parse(fmt.Sprintf("https://%s.cos.%s.myqcloud.com", bucket, region))
}

// COSUploader 封装上传文件到腾讯云COS并生成预签名URL
type COSUploader struct {
	client    *cos.Client
	secretID  string
	secretKey string
}

// NewCOSUploader 创建一个COS上传器
func NewCOSUploader(secretID, secretKey, bucket, region string) (*COSUploader, error) {
	bucketURL, err := buildBucketURL(bucket, region)
	if err != nil {
		return nil, fmt.Errorf("构造COS bucket地址失败: %w", err)
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: bucketURL}, &http.Client{
		Timeout: 30 * time.Second,
		Transport: &cos.AuthorizationTransport{
			SecretID:  secretID,
			SecretKey: secretKey,
		},
	})
	return &COSUploader{client: client, secretID: secretID, secretKey: secretKey}, nil
}

// UploadAndSign 把data上传到COS的objectKey路径下，返回一个~10分钟时效的预签名URL供QQ服务器拉取。
// bucket保持私有读写，不依赖公开读权限。
func (u *COSUploader) UploadAndSign(ctx context.Context, objectKey string, data []byte) (string, error) {
	if _, err := u.client.Object.Put(ctx, objectKey, bytes.NewReader(data), nil); err != nil {
		return "", fmt.Errorf("上传COS失败: %w", err)
	}
	signedURL, err := u.client.Object.GetPresignedURL(
		ctx, http.MethodGet, objectKey, u.secretID, u.secretKey, presignedURLExpiry, nil,
	)
	if err != nil {
		return "", fmt.Errorf("生成COS预签名URL失败: %w", err)
	}
	return signedURL.String(), nil
}
