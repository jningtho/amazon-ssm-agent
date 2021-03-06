package windows

import (
	"testing"

	c "github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/envdetect/constants"
	"github.com/stretchr/testify/assert"
)

func TestDetectPkgManager(t *testing.T) {
	d := Detector{}
	result, err := d.DetectPkgManager("", "", "") // parameters only matter for linux

	assert.NoError(t, err)
	assert.Equal(t, c.PackageManagerWindows, result)
}

func TestDetectInitSystem(t *testing.T) {
	d := Detector{}
	result, err := d.DetectInitSystem()

	assert.NoError(t, err)
	assert.Equal(t, c.InitWindows, result)
}

func TestParseVersion(t *testing.T) {
	data := []struct {
		name            string
		wmioutput       string
		expectedVersion string
		expectError     bool
	}{
		{
			"simple single line version",
			"Version=10.0.14393",
			"10.0.14393",
			false,
		},
		{
			"simple multiline line version",
			"Version=10.0.14393\n",
			"10.0.14393",
			false,
		},
		{
			"whitespace version",
			"  \t Version  \t  = \t  10.0.14393  \t",
			"10.0.14393",
			false,
		},
		{
			"multiple version",
			"CdVersion=342\nVersion=10.0.14393",
			"10.0.14393",
			false,
		},
		{
			"windows newline",
			"\r\nVersion=10.0.14393\r\n",
			"10.0.14393",
			false,
		},
		{
			"empty input",
			"",
			"",
			true,
		},
	}

	for _, d := range data {
		t.Run(d.name, func(t *testing.T) {
			resultVersion, err := parseVersion(d.wmioutput)

			if d.expectError {
				assert.True(t, err != nil, "error expected")
			} else {
				assert.NoError(t, err)
				assert.Equal(t, d.expectedVersion, resultVersion)
			}
		})
	}
}

func TestParseOperatingSystemSKU(t *testing.T) {
	data := []struct {
		name        string
		wmioutput   string
		expectedSKU string
		expectError bool
	}{
		{
			"simple single line SKU",
			"OperatingSystemSKU=7",
			"7",
			false,
		},
		{
			"simple multiline line SKU",
			"OperatingSystemSKU=7\n",
			"7",
			false,
		},
		{
			"whitespace SKU",
			"  \t OperatingSystemSKU  \t  = \t  7  \t",
			"7",
			false,
		},
		{
			"multiple SKU",
			"SomeOtherOperatingSystemSKU=143\nOperatingSystemSKU=7",
			"7",
			false,
		},
		{
			"windows newline",
			"\r\nOperatingSystemSKU=7\r\n",
			"7",
			false,
		},
		{
			"empty input",
			"",
			"",
			true,
		},
	}

	for _, d := range data {
		t.Run(d.name, func(t *testing.T) {
			resultSKU, err := parseOperatingSystemSKU(d.wmioutput)

			if d.expectError {
				assert.True(t, err != nil, "error expected")
			} else {
				assert.NoError(t, err)
				assert.Equal(t, d.expectedSKU, resultSKU)
			}
		})
	}
}
