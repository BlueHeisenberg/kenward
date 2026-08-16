; Inno Setup script for the kenward desktop wrapper.
;
;   iscc /DVersion=0.1.0 /DSourceDir=..\..\dist\windows_amd64 packaging\windows\kenward.iss
;
; SourceDir must hold kenward-desktop.exe and kenward.exe. Both are installed, because
; kenward-desktop looks for the daemon beside its own executable: one installer, one
; directory, no PATH surgery, and an uninstall that leaves nothing behind.
;
; Inno rather than WiX or MSI: this installs two files and a shortcut, it needs no
; elevation, and the whole configuration is the forty readable lines below. An MSI
; would buy per-machine deployment and Group Policy, which no household wants.
;
; Nothing here is code-signed. That is a scope decision with a consequence the user
; must be warned about rather than discover — see the SmartScreen note in
; docs/DESKTOP.md and the message shown on the finished page below.

#ifndef Version
  #define Version "0.0.0-dev"
#endif
#ifndef SourceDir
  #define SourceDir "..\..\dist\windows_amd64"
#endif

[Setup]
AppId={{6E7A2C1E-4E1F-4E5C-9E77-9A3B7C2F1D40}
AppName=kenward
AppVersion={#Version}
AppPublisher=BlueHeisenberg
DefaultDirName={autopf}\kenward
DefaultGroupName=kenward
DisableProgramGroupPage=yes
UninstallDisplayIcon={app}\kenward-desktop.exe
OutputDir=..\..\dist
OutputBaseFilename=kenward_{#Version}_windows_amd64_setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
; Per-user, so no UAC prompt and no administrator. A household assistant that needs an
; administrator to install is one that does not get installed.
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible

[Files]
Source: "{#SourceDir}\kenward-desktop.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourceDir}\kenward.exe";         DestDir: "{app}"; Flags: ignoreversion
Source: "..\kenward.ico";                   DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\kenward"; Filename: "{app}\kenward-desktop.exe"; IconFilename: "{app}\kenward.ico"

[Tasks]
; Offered, never assumed. The same setting is a checkbox in the tray's Status submenu,
; and both write the same HKCU Run value, so turning it off in either place turns it
; off in both.
Name: "startup"; Description: "Start kenward when I sign in"; GroupDescription: "Startup"; Flags: unchecked

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; \
    ValueName: "kenward-desktop"; ValueData: """{app}\kenward-desktop.exe"""; \
    Flags: uninsdeletevalue; Tasks: startup

[Run]
Filename: "{app}\kenward-desktop.exe"; Description: "Start kenward now"; Flags: nowait postinstall skipifsilent

[Messages]
; Shown on the last page, where somebody will actually read it.
FinishedLabel=kenward is installed.%n%nThe tray icon is green when the daemon is running, grey when it is stopped and red when it has failed. Windows 11 hides new tray icons: click the ^ chevron beside the clock and drag kenward out of the overflow if you want it always visible.%n%nThis installer is not code-signed, which is why SmartScreen warned you. Run `kenward doctor` from a terminal at any time for the same report the Status menu shows.
