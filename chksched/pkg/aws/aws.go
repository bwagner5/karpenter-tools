package aws

import (
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/mitchellh/go-homedir"
	"gopkg.in/ini.v1"
)

const (
	awsRegionEnvVar     = "AWS_REGION"
	defaultRegionEnvVar = "AWS_DEFAULT_REGION"
	defaultProfile      = "default"
	awsConfigFile       = "~/.aws/config"
)

func GetSession(regionName *string, profileName *string) (*session.Session, error) {
	errorMsg := ""
	sessOpts := session.Options{SharedConfigState: session.SharedConfigEnable}
	if regionName != nil {
		sessOpts.Config.Region = regionName
	}

	if profileName != nil {
		sessOpts.Profile = *profileName
		if sessOpts.Config.Region == nil {
			if profileRegion, err := getProfileRegion(*profileName); err != nil {
				errorMsg = errorMsg + err.Error()
			} else {
				sessOpts.Config.Region = &profileRegion
			}
		}
	}

	sess := session.Must(session.NewSessionWithOptions(sessOpts))
	if sess.Config.Region != nil && *sess.Config.Region != "" {
		return sess, nil
	}
	if defaultProfileRegion, err := getProfileRegion(defaultProfile); err == nil {
		sess.Config.Region = &defaultProfileRegion
		return sess, nil
	}

	if defaultRegion, ok := os.LookupEnv(defaultRegionEnvVar); ok && defaultRegion != "" {
		sess.Config.Region = &defaultRegion
		return sess, nil
	}

	errorMsg = errorMsg + "Unable to find a region in the usual places: \n"
	errorMsg = errorMsg + "\t - --region flag\n"
	errorMsg = errorMsg + fmt.Sprintf("\t - %s environment variable\n", awsRegionEnvVar)
	if profileName != nil {
		errorMsg = errorMsg + fmt.Sprintf("\t - profile region in %s\n", awsConfigFile)
	}
	errorMsg = errorMsg + fmt.Sprintf("\t - default profile region in %s\n", awsConfigFile)
	errorMsg = errorMsg + fmt.Sprintf("\t - %s environment variable\n", defaultRegionEnvVar)
	return sess, fmt.Errorf(errorMsg)
}

func getProfileRegion(profileName string) (string, error) {
	if profileName != defaultProfile {
		profileName = fmt.Sprintf("profile %s", profileName)
	}
	awsConfigPath, err := homedir.Expand(awsConfigFile)
	if err != nil {
		return "", fmt.Errorf("Warning: unable to find home directory to parse aws config file")
	}
	awsConfigIni, err := ini.Load(awsConfigPath)
	if err != nil {
		return "", fmt.Errorf("Warning: unable to load aws config file for profile at path: %s", awsConfigPath)
	}
	section, err := awsConfigIni.GetSection(profileName)
	if err != nil {
		return "", fmt.Errorf("Warning: there is no configuration for the specified aws profile %s at %s", profileName, awsConfigPath)
	}
	regionConfig, err := section.GetKey("region")
	if err != nil || regionConfig.String() == "" {
		return "", fmt.Errorf("Warning: there is no region configured for the specified aws profile %s at %s", profileName, awsConfigPath)
	}
	return regionConfig.String(), nil
}
