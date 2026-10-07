package ebs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// An AMI names its root device, and the snapshot behind it. One EC2
// DescribeImages request finds it: Range signs that request itself rather
// than take on the EC2 SDK, which is larger than the rest of Range.

// describeImages is the part of EC2's answer Range reads.
type describeImages struct {
	Images []struct {
		ID         string `xml:"imageId"`
		RootDevice string `xml:"rootDeviceName"`
		Devices    []struct {
			Name     string `xml:"deviceName"`
			Snapshot string `xml:"ebs>snapshotId"`
		} `xml:"blockDeviceMapping>item"`
	} `xml:"imagesSet>item"`
	Errors []struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Errors>Error"`
}

// rootSnapshot returns the snapshot behind an AMI's root device.
func rootSnapshot(ctx context.Context, cfg aws.Config, ami string) (string, error) {
	form := url.Values{"Action": {"DescribeImages"}, "Version": {"2016-11-15"}, "ImageId.1": {ami}}.Encode()
	endpoint := fmt.Sprintf("https://ec2.%s.amazonaws.com/", cfg.Region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: AWS credentials: %w", ami, err)
	}
	sum := sha256.Sum256([]byte(form))
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "ec2", cfg.Region, time.Now()); err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: describe the image: %w", ami, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%s: describe the image: %w", ami, err)
	}
	return parseRootSnapshot(ami, body)
}

func parseRootSnapshot(ami string, body []byte) (string, error) {
	var out describeImages
	if err := xml.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("%s: EC2 gave an answer Range cannot read: %w", ami, err)
	}
	if len(out.Errors) > 0 {
		return "", fmt.Errorf("%s: %s: %s", ami, out.Errors[0].Code, out.Errors[0].Message)
	}
	for _, image := range out.Images {
		for _, device := range image.Devices {
			if image.ID == ami && device.Name == image.RootDevice && device.Snapshot != "" {
				return device.Snapshot, nil
			}
		}
	}
	return "", fmt.Errorf("%s: no such image here, or its root device is not an EBS snapshot", ami)
}
