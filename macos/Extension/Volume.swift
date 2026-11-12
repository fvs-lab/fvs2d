import Foundation
import FSKit
import Darwin

final class Item: FSItem {
    let id: UInt64
    var path: String
    let type: FSItem.ItemType
    var handle: UInt64?
    var writable = false
    var readable = false

    init(entry: Entry, path: String) {
        id = entry.id
        self.path = path
        type = itemType(entry.mode)
        super.init()
    }
}

func itemType(_ mode: UInt32) -> FSItem.ItemType {
    switch mode & UInt32(S_IFMT) {
    case UInt32(S_IFDIR): return .directory
    case UInt32(S_IFLNK): return .symlink
    default: return .file
    }
}

func itemID(_ id: UInt64) -> FSItem.Identifier {
    if id == 1 { return .rootDirectory }
    return FSItem.Identifier(rawValue: id + 1) ?? .invalid
}

final class FVSVolume: FSVolume, FSVolume.Operations, FSVolume.ReadWriteOperations, FSVolume.OpenCloseOperations {
    private let bridge: Bridge
    private let queue = DispatchQueue(label: "lab.fvs.FVS.volume")
    private let root: Item
    private var items: [UInt64: Item] = [:]
    private var generation: UInt64 = 1
    private var closed = false

    init(source: String) throws {
        bridge = try Bridge(source: source)
        guard let entry = try bridge.call("lookup").attr else { throw POSIXError(.EIO) }
        root = Item(entry: entry, path: "")
        super.init(volumeID: FSVolume.Identifier(uuid: UUID()), volumeName: FSFileName(string: "FVS"))
        items[root.id] = root
    }

    private func item(_ value: FSItem) throws -> Item {
        guard let result = value as? Item, items[result.id] === result else { throw POSIXError(.ESTALE) }
        return result
    }

    private func path(_ name: FSFileName, in directory: FSItem) throws -> String {
        let parent = try item(directory)
        guard parent.type == .directory else { throw POSIXError(.ENOTDIR) }
        guard let component = String(data: name.data, encoding: .utf8), !component.isEmpty,
              component != ".", component != "..", !component.contains("/"), !component.contains("\0") else {
            throw POSIXError(.EILSEQ)
        }
        return parent.path.isEmpty ? component : parent.path + "/" + component
    }

    private func cached(_ entry: Entry, path: String) -> Item {
        if let result = items[entry.id] { return result }
        let result = Item(entry: entry, path: path)
        items[entry.id] = result
        return result
    }

    private func attributes(_ entry: Entry) -> FSItem.Attributes {
        let result = FSItem.Attributes()
        result.fileID = itemID(entry.id)
        result.type = itemType(entry.mode)
        result.mode = entry.mode & 0o7777
        result.size = UInt64(max(0, entry.size))
        result.uid = entry.uid
        result.gid = entry.gid
        result.linkCount = entry.mode & UInt32(S_IFMT) == UInt32(S_IFDIR) ? 2 : 1
        var seconds = entry.mtimeNS / 1_000_000_000
        var nanoseconds = entry.mtimeNS % 1_000_000_000
        if nanoseconds < 0 { seconds -= 1; nanoseconds += 1_000_000_000 }
        result.modifyTime = timespec(tv_sec: Int(seconds), tv_nsec: Int(nanoseconds))
        return result
    }

    private func entry(_ item: Item) throws -> Entry {
        let response = try item.handle.map { try bridge.call("stat", fields: ["id": $0]) }
            ?? bridge.call("lookup", path: item.path)
        guard let result = response.attr else { throw POSIXError(.EIO) }
        return result
    }

    private func open(_ item: Item, write: Bool, read: Bool) throws -> UInt64 {
        if let handle = item.handle, (!write || item.writable) && (!read || item.readable) { return handle }
        let needsWrite = write || item.writable
        let needsRead = read || item.readable
        guard let newHandle = try bridge.call("open", path: item.path,
                                             fields: ["write": needsWrite, "write_only": needsWrite && !needsRead]).handle else {
            throw POSIXError(.EIO)
        }
        if let old = item.handle { _ = try bridge.call("close", fields: ["id": old]) }
        item.handle = newHandle
        item.writable = needsWrite
        item.readable = needsRead
        return newHandle
    }

    private func timestamp(_ value: timespec) throws -> Int64 {
        guard value.tv_nsec >= 0 && value.tv_nsec < 1_000_000_000 else { throw POSIXError(.EINVAL) }
        let (seconds, overflow) = Int64(value.tv_sec).multipliedReportingOverflow(by: 1_000_000_000)
        let (nanoseconds, additionOverflow) = seconds.addingReportingOverflow(Int64(value.tv_nsec))
        guard !overflow && !additionOverflow else { throw POSIXError(.EOVERFLOW) }
        return nanoseconds
    }

    private func changes(_ request: FSItem.SetAttributesRequest, creating: Bool = false) throws -> [String: Any] {
        if request.isValid(.flags) || request.isValid(.birthTime) || request.isValid(.backupTime) || request.isValid(.addedTime) {
            throw POSIXError(.ENOTSUP)
        }
        if !creating && (request.isValid(.type) || request.isValid(.linkCount) || request.isValid(.allocSize) || request.isValid(.fileID) || request.isValid(.parentID) || request.isValid(.changeTime)) {
            throw POSIXError(.EINVAL)
        }
        var result: [String: Any] = [:]
        if request.isValid(.mode) {
            guard request.mode & ~UInt32(0o7777) == 0 else { throw POSIXError(.EINVAL) }
            result["mode"] = request.mode
        }
        if request.isValid(.size) {
            guard request.size <= UInt64(Int64.max) else { throw POSIXError(.EFBIG) }
            result["size"] = request.size
        }
        if request.isValid(.uid) { result["uid"] = request.uid }
        if request.isValid(.gid) { result["gid"] = request.gid }
        if request.isValid(.modifyTime) { result["mtime_ns"] = try timestamp(request.modifyTime) }
        if request.isValid(.accessTime) { result["atime_ns"] = try timestamp(request.accessTime) }
        return result
    }

    private func create(_ child: String, kind: String, target: String = "", mode: Any,
                        changes: [String: Any]) throws -> Item {
        let response = try bridge.call("create", path: child,
                                       fields: ["kind": kind, "target": target, "mode": mode])
        guard var entry = response.attr else {
            throw POSIXError(.EIO)
        }
        do {
            if !changes.isEmpty {
                var fields: [String: Any] = ["changes": changes]
                if let handle = response.handle { fields["id"] = handle }
                guard let updated = try bridge.call("setattr", path: child, fields: fields).attr else {
                    throw POSIXError(.EIO)
                }
                entry = updated
            }
        } catch {
            let failure = error
            do {
                if let handle = response.handle { _ = try bridge.call("close", fields: ["id": handle]) }
                _ = try bridge.call("remove", path: child, fields: ["kind": kind])
            } catch {
                throw NSError(domain: NSPOSIXErrorDomain, code: Int(EIO), userInfo: [
                    NSLocalizedDescriptionKey: "Creation attributes failed: \(failure.localizedDescription). Removing the incomplete item failed: \(error.localizedDescription)",
                    NSUnderlyingErrorKey: failure
                ])
            }
            throw failure
        }
        generation &+= 1
        let result = cached(entry, path: child)
        result.handle = response.handle
        result.writable = response.handle != nil
        result.readable = response.handle != nil
        return result
    }

    var volumeStatistics: FSStatFSResult {
        queue.sync {
            let result = FSStatFSResult(fileSystemTypeName: "fvs")
            result.blockSize = 4096
            result.ioSize = 1 << 20
            if let response = try? bridge.call("statfs"), let statistics = response.statistics {
                result.blockSize = Int(statistics.block_size)
                result.ioSize = Int(statistics.io_size)
                result.totalBlocks = statistics.total_blocks
                result.freeBlocks = statistics.free_blocks
                result.availableBlocks = statistics.available_blocks
                result.usedBlocks = statistics.total_blocks - min(statistics.total_blocks, statistics.free_blocks)
                result.totalFiles = statistics.total_files
                result.freeFiles = statistics.free_files
            }
            return result
        }
    }

    var supportedVolumeCapabilities: FSVolume.SupportedCapabilities {
        let caps = FSVolume.SupportedCapabilities()
        caps.caseFormat = .sensitive
        caps.supportsSymbolicLinks = true
        caps.supportsHardLinks = false
        caps.supportsPersistentObjectIDs = false
        caps.supports64BitObjectIDs = true
        caps.doesNotSupportVolumeSizes = true
        return caps
    }

    var maximumLinkCount: Int { 1 }
    var maximumNameLength: Int { 255 }
    var restrictsOwnershipChanges: Bool { true }
    var truncatesLongNames: Bool { false }
    var maximumFileSizeInBits: Int { 64 }
    var maximumXattrSizeInBits: Int { 0 }

    func activate(options: FSTaskOptions, replyHandler: @escaping (FSItem?, Error?) -> Void) { replyHandler(root, nil) }
    func mount(options: FSTaskOptions, replyHandler: @escaping (Error?) -> Void) { replyHandler(nil) }
    func unmount(replyHandler: @escaping () -> Void) { replyHandler() }
    func shutdown() throws {
        try queue.sync {
            if closed { return }
            var failure: Error?
            do { _ = try bridge.call("sync") } catch { failure = error }
            for value in items.values {
                if let handle = value.handle {
                    do { _ = try bridge.call("close", fields: ["id": handle]) }
                    catch { if failure == nil { failure = error } }
                    value.handle = nil
                }
            }
            do { try bridge.close() } catch { if failure == nil { failure = error } }
            closed = true
            if let failure = failure { throw failure }
        }
    }

    func deactivate(options: FSDeactivateOptions = [], replyHandler: @escaping (Error?) -> Void) {
        do { try shutdown(); replyHandler(nil) } catch { replyHandler(error) }
    }
    func synchronize(flags: FSSyncFlags, replyHandler: @escaping (Error?) -> Void) {
        queue.sync { do { _ = try bridge.call("sync"); replyHandler(nil) } catch { replyHandler(error) } }
    }
    func getAttributes(_ desiredAttributes: FSItem.GetAttributesRequest, of value: FSItem,
                       replyHandler: @escaping (FSItem.Attributes?, Error?) -> Void) {
        queue.sync { do { replyHandler(attributes(try entry(item(value))), nil) } catch { replyHandler(nil, error) } }
    }
    func setAttributes(_ request: FSItem.SetAttributesRequest, on value: FSItem,
                       replyHandler: @escaping (FSItem.Attributes?, Error?) -> Void) {
        queue.sync {
            do {
                let value = try item(value)
                var fields: [String: Any] = ["changes": try changes(request)]
                if let handle = value.handle { fields["id"] = handle }
                let response = try bridge.call("setattr", path: value.path, fields: fields)
                guard let attr = response.attr else { throw POSIXError(.EIO) }
                replyHandler(attributes(attr), nil)
            } catch { replyHandler(nil, error) }
        }
    }
    func lookupItem(named name: FSFileName, inDirectory directory: FSItem,
                    replyHandler: @escaping (FSItem?, FSFileName?, Error?) -> Void) {
        queue.sync {
            do {
                let child = try path(name, in: directory)
                guard let attr = try bridge.call("lookup", path: child).attr else { throw POSIXError(.EIO) }
                replyHandler(cached(attr, path: child), name, nil)
            } catch { replyHandler(nil, nil, error) }
        }
    }
    func reclaimItem(_ value: FSItem, replyHandler: @escaping (Error?) -> Void) {
        queue.sync {
            do { let value = try item(value); if let handle = value.handle { _ = try bridge.call("close", fields: ["id": handle]); value.handle = nil }; items.removeValue(forKey: value.id); replyHandler(nil) }
            catch { replyHandler(error) }
        }
    }
    func readSymbolicLink(_ value: FSItem, replyHandler: @escaping (FSFileName?, Error?) -> Void) {
        queue.sync {
            do { guard let target = try bridge.call("readlink", path: item(value).path).target else { throw POSIXError(.EIO) }; replyHandler(FSFileName(data: Data(target.utf8)), nil) }
            catch { replyHandler(nil, error) }
        }
    }
    func createItem(named name: FSFileName, type: FSItem.ItemType, inDirectory directory: FSItem,
                    attributes request: FSItem.SetAttributesRequest,
                    replyHandler: @escaping (FSItem?, FSFileName?, Error?) -> Void) {
        queue.sync {
            do {
                guard type == .file || type == .directory else { throw POSIXError(.ENOTSUP) }
                let child = try path(name, in: directory)
                var fields = try changes(request, creating: true)
                let mode = fields.removeValue(forKey: "mode") ?? (type == .directory ? 0o755 : 0o644)
                let created = try create(child, kind: type == .directory ? "dir" : "file", mode: mode, changes: fields)
                replyHandler(created, name, nil)
            } catch { replyHandler(nil, nil, error) }
        }
    }
    func createSymbolicLink(named name: FSFileName, inDirectory directory: FSItem,
                            attributes request: FSItem.SetAttributesRequest, linkContents contents: FSFileName,
                            replyHandler: @escaping (FSItem?, FSFileName?, Error?) -> Void) {
        queue.sync {
            do {
                var fields = try changes(request, creating: true)
                if let mode = fields.removeValue(forKey: "mode") as? UInt32, mode != 0o777 { throw POSIXError(.ENOTSUP) }
                guard let target = String(data: contents.data, encoding: .utf8) else { throw POSIXError(.EILSEQ) }
                let child = try path(name, in: directory)
                let attr = try create(child, kind: "symlink", target: target, mode: 0o777, changes: fields)
                replyHandler(attr, name, nil)
            } catch { replyHandler(nil, nil, error) }
        }
    }
    func createLink(to item: FSItem, named name: FSFileName, inDirectory directory: FSItem,
                    replyHandler: @escaping (FSFileName?, Error?) -> Void) { replyHandler(nil, POSIXError(.ENOTSUP)) }
    func removeItem(_ value: FSItem, named name: FSFileName, fromDirectory directory: FSItem,
                    replyHandler: @escaping (Error?) -> Void) {
        queue.sync {
            do { let value = try item(value); let child = try path(name, in: directory); _ = try bridge.call("remove", path: child, fields: ["kind": value.type == .directory ? "dir" : "file"]); generation &+= 1; replyHandler(nil) }
            catch { replyHandler(error) }
        }
    }
    func renameItem(_ value: FSItem, inDirectory sourceDirectory: FSItem, named sourceName: FSFileName,
                    to destinationName: FSFileName, inDirectory destinationDirectory: FSItem, overItem: FSItem?,
                    replyHandler: @escaping (FSFileName?, Error?) -> Void) {
        queue.sync {
            do {
                _ = try item(value)
                let source = try path(sourceName, in: sourceDirectory)
                let destination = try path(destinationName, in: destinationDirectory)
                _ = try bridge.call("rename", path: source, fields: ["target": destination])
                let sourceBytes = Array(source.utf8)
                let prefix = sourceBytes + [UInt8(47)]
                let destinationBytes = Array(destination.utf8)
                for item in items.values {
                    let bytes = Array(item.path.utf8)
                    if bytes == sourceBytes || bytes.starts(with: prefix) {
                        item.path = String(decoding: destinationBytes + Array(bytes.dropFirst(sourceBytes.count)), as: UTF8.self)
                    }
                }

                generation &+= 1
                replyHandler(destinationName, nil)
            } catch { replyHandler(nil, error) }
        }
    }
    func enumerateDirectory(_ directory: FSItem, startingAt cookie: FSDirectoryCookie,
                            verifier: FSDirectoryVerifier, attributes request: FSItem.GetAttributesRequest?,
                            packer: FSDirectoryEntryPacker, replyHandler: @escaping (FSDirectoryVerifier, Error?) -> Void) {
        queue.sync {
            do {
                let directory = try item(directory)
                if cookie.rawValue != 0 && verifier.rawValue != generation { throw POSIXError(.ESTALE) }
                let entries = try bridge.call("readdir", path: directory.path).entries ?? []
                guard cookie.rawValue <= UInt64(entries.count) else { throw POSIXError(.EINVAL) }
                for index in Int(cookie.rawValue)..<entries.count {
                    let entry = entries[index]
                    guard let name = entry.name else { throw POSIXError(.EIO) }
                    if !packer.packEntry(name: FSFileName(data: Data(name.utf8)), itemType: itemType(entry.mode),
                                         itemID: itemID(entry.id), nextCookie: FSDirectoryCookie(UInt64(index + 1)),
                                         attributes: request == nil ? nil : attributes(entry)) { break }
                }
                replyHandler(FSDirectoryVerifier(generation), nil)
            } catch { replyHandler(FSDirectoryVerifier(0), error) }
        }
    }
    func openItem(_ value: FSItem, modes: FSVolume.OpenModes, replyHandler: @escaping (Error?) -> Void) {
        queue.sync { do { let value = try item(value); if value.type == .file { _ = try open(value, write: modes.contains(.write), read: modes.contains(.read)) }; replyHandler(nil) } catch { replyHandler(error) } }
    }
    func closeItem(_ value: FSItem, modes: FSVolume.OpenModes, replyHandler: @escaping (Error?) -> Void) {
        // FSKit may still issue page-in reads for an mmap after close. The handle
        // belongs to the FSItem and is released by reclaimItem or deactivate.
        replyHandler(nil)
    }
    func read(from value: FSItem, at offset: off_t, length: Int, into buffer: FSMutableFileDataBuffer,
              replyHandler: @escaping (Int, Error?) -> Void) {
        do {
            let handle = try queue.sync { try open(item(value), write: false, read: true) }
            var count = 0
            try buffer.withUnsafeMutableBytes { bytes in
                count = try bridge.read(handle, buffer: bytes, length: length, offset: offset)
                if count < length {
                    bytes.baseAddress?.advanced(by: count).initializeMemory(as: UInt8.self, repeating: 0, count: length - count)
                }
            }
            replyHandler(count, nil)
        } catch { replyHandler(0, error) }
    }
    func write(contents: Data, to value: FSItem, at offset: off_t, replyHandler: @escaping (Int, Error?) -> Void) {
        do { let handle = try queue.sync { try open(item(value), write: true, read: false) }; replyHandler(try bridge.write(handle, data: contents, offset: offset), nil) }
        catch { replyHandler(0, error) }
    }
}
