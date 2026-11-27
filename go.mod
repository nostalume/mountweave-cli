module github.com/nostalume/mountweave-cli

go 1.27.0

require (
	github.com/Azure/azure-sdk-for-go/sdk/storage/azblob v1.8.2
	github.com/aws/aws-sdk-go-v2 v1.47.5
	github.com/aws/aws-sdk-go-v2/config v1.32.30
	github.com/aws/aws-sdk-go-v2/credentials v1.19.29
	github.com/aws/aws-sdk-go-v2/service/s3 v1.114.6
	github.com/aws/smithy-go v1.28.7
	github.com/chromedp/cdproto v0.157.8
	github.com/chromedp/chromedp v0.20.0
	github.com/hirochachacha/go-smb2/v2 v2.0.0-20261011164520-7f69f6587832
	github.com/nostalume/mountweave v0.4.0
	github.com/nostalume/mountweave/browser v0.2.0
	github.com/nostalume/mountweave/drivers/alipan v0.5.0
	github.com/nostalume/mountweave/drivers/atomgit v0.1.0
	github.com/nostalume/mountweave/drivers/azureblob v0.1.0
	github.com/nostalume/mountweave/drivers/baidupan v0.2.0
	github.com/nostalume/mountweave/drivers/baidupan/cdp v0.1.0
	github.com/nostalume/mountweave/drivers/box v0.1.0
	github.com/nostalume/mountweave/drivers/bunny v0.1.0
	github.com/nostalume/mountweave/drivers/cloudreve v0.1.0
	github.com/nostalume/mountweave/drivers/cnbreleases v0.1.0
	github.com/nostalume/mountweave/drivers/codeartsrepo v0.1.0
	github.com/nostalume/mountweave/drivers/dingtalkdrive v0.1.0
	github.com/nostalume/mountweave/drivers/dropbox v0.3.0
	github.com/nostalume/mountweave/drivers/fangcloud v0.1.0
	github.com/nostalume/mountweave/drivers/feishudrive v0.1.0
	github.com/nostalume/mountweave/drivers/ftp v0.1.0
	github.com/nostalume/mountweave/drivers/githubreleases v0.1.0
	github.com/nostalume/mountweave/drivers/githubrepo v0.1.0
	github.com/nostalume/mountweave/drivers/gokuai v0.1.0
	github.com/nostalume/mountweave/drivers/googledrive v0.3.0
	github.com/nostalume/mountweave/drivers/graphdrive v0.3.0
	github.com/nostalume/mountweave/drivers/guangyapan v0.2.0
	github.com/nostalume/mountweave/drivers/ipfs v0.1.0
	github.com/nostalume/mountweave/drivers/kdocs v0.1.0
	github.com/nostalume/mountweave/drivers/lanzou v0.1.0
	github.com/nostalume/mountweave/drivers/mediafire v0.2.0
	github.com/nostalume/mountweave/drivers/mobilepan v0.2.0
	github.com/nostalume/mountweave/drivers/pan115 v0.3.0
	github.com/nostalume/mountweave/drivers/pan115/cdp v0.1.0
	github.com/nostalume/mountweave/drivers/pan123 v0.4.0
	github.com/nostalume/mountweave/drivers/pan123/cdp v0.2.0
	github.com/nostalume/mountweave/drivers/pikpak v0.1.0
	github.com/nostalume/mountweave/drivers/pool v0.3.0
	github.com/nostalume/mountweave/drivers/quark v0.2.0
	github.com/nostalume/mountweave/drivers/quark/cdp v0.1.0
	github.com/nostalume/mountweave/drivers/s3 v0.2.0
	github.com/nostalume/mountweave/drivers/seafile v0.1.0
	github.com/nostalume/mountweave/drivers/sftp v0.1.0
	github.com/nostalume/mountweave/drivers/smb v0.1.0
	github.com/nostalume/mountweave/drivers/tencentdocs v0.1.0
	github.com/nostalume/mountweave/drivers/tencentsmh v0.1.0
	github.com/nostalume/mountweave/drivers/tianyipan v0.1.0
	github.com/nostalume/mountweave/drivers/uc v0.2.0
	github.com/nostalume/mountweave/drivers/upyun v0.1.0
	github.com/nostalume/mountweave/drivers/webdav v0.1.0
	github.com/nostalume/mountweave/drivers/wecomdrive v0.1.0
	github.com/nostalume/mountweave/drivers/weiyun v0.3.0
	github.com/nostalume/mountweave/drivers/yunpan360 v0.1.0
	github.com/nostalume/mountweave/protocols/s3 v0.1.0
	github.com/nostalume/mountweave/protocols/sftp v0.1.0
	github.com/pkg/sftp v1.13.12
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	go.etcd.io/bbolt v1.4.3
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.23.2 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.12.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/storage/internal v0.1.0 // indirect
	github.com/apache/arrow-go/v18 v18.7.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.25 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.4.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.32.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.37.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.44.1 // indirect
	github.com/geoffgarside/ber v1.1.0 // indirect
	github.com/go-crypt/x v0.4.17 // indirect
	github.com/go-json-experiment/json v0.0.0-20260213210345-44df1a37e875 // indirect
	github.com/go-krb5/krb5 v0.1.1-0.20260910042439-3e50d4009c09 // indirect
	github.com/go-krb5/x v0.3.3 // indirect
	github.com/goccy/go-json v0.10.6 // indirect
	github.com/google/flatbuffers v25.12.19+incompatible // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/klauspost/reedsolomon v1.13.3 // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.28 // indirect
	github.com/pkg/sftp/v2 v2.0.0-alpha2 // indirect
	github.com/quic-go/quic-go v0.62.0 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	golang.org/x/exp v0.0.0-20260813180055-c1d0aacb2297 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)
