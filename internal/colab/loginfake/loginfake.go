// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package loginfake writes a fake gcloud for the tests of the in-app Google
// sign-in (internal/colab login.go and the /api/colab/auth/login
// endpoints): a gcloud.cmd (Windows) or gcloud shell script (macOS, Linux)
// in a google-cloud-sdk/bin directory, like the real Cloud SDK's. It
// contacts nobody: it prints a sign-in link the way gcloud does, and then,
// depending on EnvMode, writes an ADC file into CLOUDSDK_CONFIG, fails,
// exits without a credential, or hangs until it is stopped.
package loginfake

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Environment variables the fake reads.
const (
	// EnvMode selects the behaviour (Mode*; empty = ModeSuccess).
	EnvMode = "LECTUREFLOW_FAKE_GCLOUD_MODE"
	// EnvOut is a directory where the fake records its arguments
	// (args.txt) and the gcloud variables it saw (env.txt); "" = nothing.
	EnvOut = "LECTUREFLOW_FAKE_GCLOUD_OUT"
	// EnvChild is an executable ModeHang runs (with the arguments
	// "sleeper python") instead of ping / sleep, as gcloud runs Python;
	// "" = ping (Windows) or sleep.
	EnvChild = "LECTUREFLOW_FAKE_GCLOUD_CHILD"
	// EnvBrowser is an executable ModeBrowser starts in the background
	// (with "sleeper browser") before hanging, as gcloud starts a browser
	// through the shell (Windows only).
	EnvBrowser = "LECTUREFLOW_FAKE_GCLOUD_BROWSER"
)

// Modes.
const (
	ModeSuccess = ""        // print the link, write the ADC file, exit 0
	ModeFail    = "fail"    // print the link and an ERROR line, exit 1
	ModeNoADC   = "noadc"   // print the link, exit 0 without a credential
	ModeHang    = "hang"    // print the link, wait (a child process) until stopped
	ModeBrowser = "browser" // ModeHang after starting EnvBrowser (Windows)
	ModeNoURL   = "nourl"   // write the ADC file without printing a link
)

// URL is the sign-in link the fake prints (the shape of gcloud's).
const URL = "https://accounts.google.com/o/oauth2/auth?response_type=code&client_id=32555940559.apps.googleusercontent.com" +
	"&redirect_uri=http%3A%2F%2Flocalhost%3A8085%2F&scope=openid&state=fake-state-0123&access_type=offline"

// ErrorLine is the ERROR line of ModeFail.
const ErrorLine = "ERROR: (gcloud.auth.application-default.login) There was a problem with web authentication."

// Install writes the fake under dir and returns the path of its gcloud
// executable ({dir}/google-cloud-sdk/bin/gcloud.cmd or …/gcloud). adcJSON
// is the content of the ADC file it writes (one line, without the
// characters & | < > ^ % on Windows).
func Install(dir, adcJSON string) (string, error) {
	bin := filepath.Join(dir, "google-cloud-sdk", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return "", err
	}
	if strings.ContainsAny(adcJSON, "\r\n") {
		return "", fmt.Errorf("loginfake: the ADC content must be one line")
	}
	if runtime.GOOS == "windows" {
		if strings.ContainsAny(adcJSON, "&|<>^%") {
			return "", fmt.Errorf("loginfake: the ADC content has a character cmd.exe would interpret")
		}
		p := filepath.Join(bin, "gcloud.cmd")
		return p, os.WriteFile(p, []byte(strings.ReplaceAll(cmdScript(adcJSON), "\n", "\r\n")), 0o755)
	}
	p := filepath.Join(bin, "gcloud")
	return p, os.WriteFile(p, []byte(shScript(adcJSON)), 0o755)
}

func cmdScript(adc string) string {
	url := strings.NewReplacer("%", "%%", "&", "^&").Replace(URL)
	return `@echo off
setlocal
set "OUT=%` + EnvOut + `%"
set "MODE=%` + EnvMode + `%"
if "%OUT%"=="" goto noout
>"%OUT%\args.txt" echo %*
>"%OUT%\env.txt" echo config=%CLOUDSDK_CONFIG%
>>"%OUT%\env.txt" echo prompts=%CLOUDSDK_CORE_DISABLE_PROMPTS%
>>"%OUT%\env.txt" echo usage=%CLOUDSDK_CORE_DISABLE_USAGE_REPORTING%
>>"%OUT%\env.txt" echo gac=%GOOGLE_APPLICATION_CREDENTIALS%
>>"%OUT%\env.txt" echo project=%CLOUDSDK_CORE_PROJECT%
:noout
if "%MODE%"=="nourl" goto writeadc
echo Your browser has been opened to visit: 1>&2
echo. 1>&2
echo     ` + url + ` 1>&2
echo. 1>&2
if "%MODE%"=="fail" goto fail
if "%MODE%"=="noadc" exit /b 0
if "%MODE%"=="hang" goto hang
if "%MODE%"=="browser" goto browser
:writeadc
>"%CLOUDSDK_CONFIG%\application_default_credentials.json" echo ` + adc + `
echo. 1>&2
echo Credentials saved to file: [%CLOUDSDK_CONFIG%\application_default_credentials.json] 1>&2
exit /b 0
:fail
echo ` + strings.NewReplacer("(", "^(", ")", "^)").Replace(ErrorLine) + ` 1>&2
exit /b 1
:browser
start "" /b "%` + EnvBrowser + `%" sleeper browser
:hang
if "%` + EnvChild + `%"=="" goto ping
"%` + EnvChild + `%" sleeper python
exit /b 0
:ping
ping -n 300 127.0.0.1 >nul
exit /b 0
`
}

func shScript(adc string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	return `#!/bin/sh
OUT="$` + EnvOut + `"
MODE="$` + EnvMode + `"
if [ -n "$OUT" ]; then
  echo "$*" > "$OUT/args.txt"
  {
    echo "config=$CLOUDSDK_CONFIG"
    echo "prompts=$CLOUDSDK_CORE_DISABLE_PROMPTS"
    echo "usage=$CLOUDSDK_CORE_DISABLE_USAGE_REPORTING"
    echo "gac=$GOOGLE_APPLICATION_CREDENTIALS"
    echo "project=$CLOUDSDK_CORE_PROJECT"
  } > "$OUT/env.txt"
fi
if [ "$MODE" != "nourl" ]; then
  echo "Your browser has been opened to visit:" >&2
  echo "" >&2
  echo "    "` + q(URL) + ` >&2
  echo "" >&2
fi
case "$MODE" in
fail)
  echo ` + q(ErrorLine) + ` >&2
  exit 1 ;;
noadc)
  exit 0 ;;
hang|browser)
  if [ -n "$` + EnvChild + `" ]; then
    "$` + EnvChild + `" sleeper python &
  else
    sleep 300 &
  fi
  wait
  exit 0 ;;
esac
printf '%s\n' ` + q(adc) + ` > "$CLOUDSDK_CONFIG/application_default_credentials.json"
echo "" >&2
echo "Credentials saved to file: [$CLOUDSDK_CONFIG/application_default_credentials.json]" >&2
exit 0
`
}
