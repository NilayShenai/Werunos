$ErrorActionPreference = 'Stop'
$toolsDir = "$(Split-Path -parent $MyInvocation.MyCommand.Definition)"

Write-Output "Werunos has been successfully installed. The executable is shimmed and ready for use in your command line."
