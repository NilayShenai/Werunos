import Cocoa
import UniformTypeIdentifiers

class WerunosApp: NSObject, NSApplicationDelegate {
    
    struct MountInfo {
        let name: String
        let path: String
        let mountPoint: String
        let process: Process
    }
    
    var statusItem: NSStatusItem!
    var menu: NSMenu!
    var activeMounts: [MountInfo] = []
    
    func applicationDidFinishLaunching(_ notification: Notification) {
        setupMenu()
        setupServices()
        
        // Show launch notification
        showNotification(title: "Werunos is active", body: "Select 'Mount Image File...' from the menu bar to mount ext4 volumes.")
    }
    
    func setupMenu() {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        if let button = statusItem.button {
            if #available(macOS 11.0, *) {
                if let image = NSImage(systemSymbolName: "opticaldisc", accessibilityDescription: "Werunos") {
                    image.isTemplate = true
                    button.image = image
                } else {
                    button.title = "⏏ Werunos"
                }
            } else {
                button.title = "⏏ Werunos"
            }
        }
        
        menu = NSMenu()
        statusItem.menu = menu
        updateMenu()
    }
    
    func updateMenu() {
        menu.removeAllItems()
        
        // App Title Item
        let appTitle = NSMenuItem(title: "Werunos Ext4 Manager", action: nil, keyEquivalent: "")
        appTitle.isEnabled = false
        menu.addItem(appTitle)
        
        menu.addItem(NSMenuItem.separator())
        
        // Mounted Volumes Section
        let headerItem = NSMenuItem(title: "Mounted Volumes:", action: nil, keyEquivalent: "")
        headerItem.isEnabled = false
        menu.addItem(headerItem)
        
        if activeMounts.isEmpty {
            let noMountsItem = NSMenuItem(title: "  (None)", action: nil, keyEquivalent: "")
            noMountsItem.isEnabled = false
            menu.addItem(noMountsItem)
        } else {
            for mount in activeMounts {
                let displayName = mount.name.count > 16 ? String(mount.name.prefix(13)) + "..." : mount.name
                let mountItem = NSMenuItem(title: "  \(displayName) → \(mount.mountPoint)", action: #selector(openMountInFinder(_:)), keyEquivalent: "")
                mountItem.representedObject = mount.mountPoint
                mountItem.toolTip = "Click to open in Finder\nSource: \(mount.path)"
                menu.addItem(mountItem)
                
                let ejectItem = NSMenuItem(title: "    Eject \(displayName)", action: #selector(ejectMount(_:)), keyEquivalent: "")
                ejectItem.representedObject = mount.path
                ejectItem.image = NSImage(named: NSImage.stopProgressTemplateName)
                menu.addItem(ejectItem)
            }
        }
        
        menu.addItem(NSMenuItem.separator())
        
        // Actions Section
        let mountItem = NSMenuItem(title: "Mount Image File...", action: #selector(selectAndMountImage(_:)), keyEquivalent: "o")
        menu.addItem(mountItem)
        
        menu.addItem(NSMenuItem.separator())
        
        // System Actions
        let aboutItem = NSMenuItem(title: "About Werunos", action: #selector(showAbout(_:)), keyEquivalent: "")
        menu.addItem(aboutItem)
        
        let quitItem = NSMenuItem(title: "Quit Werunos", action: #selector(quitApp(_:)), keyEquivalent: "q")
        menu.addItem(quitItem)
    }
    
    @objc func openMountInFinder(_ sender: NSMenuItem) {
        if let mountPoint = sender.representedObject as? String {
            NSWorkspace.shared.selectFile(nil, inFileViewerRootedAtPath: mountPoint)
        }
    }
    
    @objc func ejectMount(_ sender: NSMenuItem) {
        if let path = sender.representedObject as? String {
            unmount(path: path)
        }
    }
    
    @objc func selectAndMountImage(_ sender: NSMenuItem) {
        let openPanel = NSOpenPanel()
        openPanel.title = "Select Ext4 Disk Image"
        openPanel.canChooseFiles = true
        openPanel.canChooseDirectories = false
        openPanel.allowsMultipleSelection = false
        
        if #available(macOS 11.0, *) {
            openPanel.allowedContentTypes = [UTType.image, UTType(filenameExtension: "img"), UTType(filenameExtension: "raw")].compactMap { $0 }
        } else {
            openPanel.allowedFileTypes = ["img", "raw"]
        }
        
        if openPanel.runModal() == .OK {
            if let url = openPanel.url {
                mount(path: url.path)
            }
        }
    }
    
    @objc func showAbout(_ sender: NSMenuItem) {
        let alert = NSAlert()
        alert.messageText = "About Werunos"
        alert.informativeText = "Werunos Ext4 Driver & Manager for macOS.\nPowered by userspace ext4 parser and macFUSE.\n\nAll mounts run as background user services."
        alert.alertStyle = .informational
        alert.addButton(withTitle: "OK")
        alert.runModal()
    }
    
    @objc func quitApp(_ sender: NSMenuItem) {
        // Unmount all active mounts before quitting
        for mount in activeMounts {
            mount.process.terminate()
        }
        NSApplication.shared.terminate(self)
    }
    
    // File handler for double-click
    func application(_ sender: NSApplication, openFile filename: String) -> Bool {
        if filename.hasSuffix(".img") || filename.hasSuffix(".raw") {
            mount(path: filename)
            return true
        }
        return false
    }
    
    // Setup Services (Finder Context Menu)
    func setupServices() {
        NSApp.servicesProvider = self
        NSUpdateDynamicServices()
    }
    
    // Service callback
    @objc func mountImageService(_ pasteboard: NSPasteboard, userData: String, error: AutoreleasingUnsafeMutablePointer<NSString>) {
        let filenamesType = NSPasteboard.PasteboardType("NSFilenamesPboardType")
        if pasteboard.types?.contains(filenamesType) == true {
            if let files = pasteboard.propertyList(forType: filenamesType) as? [String] {
                for file in files {
                    mount(path: file)
                }
            }
        }
    }
    
    // Mount implementation
    func mount(path: String) {
        if activeMounts.contains(where: { $0.path == path }) {
            showNotification(title: "Werunos Manager", body: "Image is already mounted.")
            return
        }
        
        let bundlePath = Bundle.main.bundlePath
        let werunosPath = (bundlePath as NSString).appendingPathComponent("Contents/MacOS/werunos")
        
        let process = Process()
        process.launchPath = werunosPath
        process.arguments = ["mount", path]
        
        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = pipe
        
        do {
            try process.run()
        } catch {
            showNotification(title: "Werunos Mount Failed", body: "Could not launch mounting helper: \(error.localizedDescription)")
            return
        }
        
        DispatchQueue.global(qos: .userInitiated).async {
            let fileHandle = pipe.fileHandleForReading
            var buffer = ""
            var volumeName = "ext4"
            var mountPoint = ""
            var success = false
            var didMount = false
            
            while true {
                let data = fileHandle.availableData
                if data.isEmpty { break }
                if let str = String(data: data, encoding: .utf8) {
                    buffer += str
                    let lines = buffer.components(separatedBy: "\n")
                    if lines.count > 1 {
                        for i in 0..<(lines.count - 1) {
                            let line = lines[i]
                            NSLog("[werunos stdout] %@", line)
                            
                            if line.contains("ext4 volume:") {
                                if let startIdx = line.range(of: "volume:")?.upperBound {
                                    var namePart = line[startIdx...].trimmingCharacters(in: .whitespacesAndNewlines)
                                    namePart = namePart.replacingOccurrences(of: "\"", with: "")
                                    if let idx = namePart.range(of: " (sanitized)")?.lowerBound {
                                        volumeName = String(namePart[..<idx])
                                    } else {
                                        volumeName = namePart
                                    }
                                }
                            }
                            if line.contains("Mounting at") {
                                if let startIdx = line.range(of: "Mounting at")?.upperBound {
                                    let remainder = line[startIdx...].trimmingCharacters(in: .whitespacesAndNewlines)
                                    if let endIdx = remainder.range(of: "…")?.lowerBound {
                                        mountPoint = remainder[..<endIdx].trimmingCharacters(in: .whitespacesAndNewlines)
                                    } else if let endIdx = remainder.range(of: " ")?.lowerBound {
                                        mountPoint = remainder[..<endIdx].trimmingCharacters(in: .whitespacesAndNewlines)
                                    } else {
                                        mountPoint = remainder
                                    }
                                    success = true
                                }
                            }
                            if line.contains("mount failed") {
                                success = false
                                break
                            }
                        }
                        buffer = lines.last ?? ""
                    }
                    
                    if success && !mountPoint.isEmpty {
                        let finalVolName = volumeName
                        let finalMountPoint = mountPoint
                        didMount = true
                        DispatchQueue.main.async {
                            self.addActiveMount(name: finalVolName, path: path, mountPoint: finalMountPoint, process: process)
                        }
                        success = false
                        mountPoint = ""
                    }
                }
            }
            
            process.waitUntilExit()
            
            DispatchQueue.main.async {
                if didMount {
                    self.removeActiveMount(path: path)
                } else {
                    let filename = (path as NSString).lastPathComponent
                    self.showNotification(title: "Werunos Mount Failed", body: "Could not mount '\(filename)'. Please verify macFUSE installations and permissions.")
                }
            }
        }
    }
    
    func unmount(path: String) {
        if let mount = activeMounts.first(where: { $0.path == path }) {
            mount.process.terminate()
        }
    }
    
    func addActiveMount(name: String, path: String, mountPoint: String, process: Process) {
        let mount = MountInfo(name: name, path: path, mountPoint: mountPoint, process: process)
        activeMounts.append(mount)
        updateMenu()
        showNotification(title: "Volume Mounted", body: "Successfully mounted \(name) at \(mountPoint)")
    }
    
    func removeActiveMount(path: String) {
        if let idx = activeMounts.firstIndex(where: { $0.path == path }) {
            let mount = activeMounts[idx]
            activeMounts.remove(at: idx)
            updateMenu()
            showNotification(title: "Volume Ejected", body: "Unmounted \(mount.name)")
        }
    }
    
    func showNotification(title: String, body: String) {
        let notification = NSUserNotification()
        notification.title = title
        notification.informativeText = body
        notification.soundName = NSUserNotificationDefaultSoundName
        NSUserNotificationCenter.default.deliver(notification)
    }
    
    func applicationWillTerminate(_ notification: Notification) {
        // Clean termination of all processes
        for mount in activeMounts {
            mount.process.terminate()
        }
    }
}

// Main launch
let app = NSApplication.shared
let delegate = WerunosApp()
app.delegate = delegate
app.run()
