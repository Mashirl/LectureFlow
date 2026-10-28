# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

# NSIS additions (electron-builder nsis.include). The app registers the
# browser extension's native messaging host at every start (internal/nmhost):
# three HKCU keys, the manifests and launch.json in %APPDATA%\LectureFlow\nmhost,
# and for the portable build a copy of the host in %LOCALAPPDATA%\LectureFlow\nmhost.
# Uninstalling removes them. Not during an update: the old version's
# uninstaller runs then, and the new version registers again at its start.
# The data directory is never touched (deleteAppDataOnUninstall: false).

!macro customUnInstall
  ${ifNot} ${isUpdated}
    DeleteRegKey HKCU "Software\Google\Chrome\NativeMessagingHosts\org.lectureflow.nmhost"
    DeleteRegKey HKCU "Software\Microsoft\Edge\NativeMessagingHosts\org.lectureflow.nmhost"
    DeleteRegKey HKCU "Software\Mozilla\NativeMessagingHosts\org.lectureflow.nmhost"
    RMDir /r "$APPDATA\LectureFlow\nmhost"
    RMDir /r "$LOCALAPPDATA\LectureFlow\nmhost"
  ${endIf}
!macroend
