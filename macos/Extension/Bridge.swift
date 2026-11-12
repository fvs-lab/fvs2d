import Foundation
import Darwin

struct Entry: Decodable {
    let id: UInt64
    let mode: UInt32
    let size: Int64
    let uid: UInt32
    let gid: UInt32
    let mtimeNS: Int64
    let name: String?

    enum CodingKeys: String, CodingKey {
        case id, mode, size, uid, gid, name
        case mtimeNS = "mtime_ns"
    }
}

struct Statistics: Decodable {
    let block_size: Int64
    let io_size: Int64
    let total_blocks: UInt64
    let free_blocks: UInt64
    let available_blocks: UInt64
    let total_files: UInt64
    let free_files: UInt64
}

struct Reply: Decodable {
    let errno: Int32
    let message: String?
    let attr: Entry?
    let entries: [Entry]?
    let target: String?
    let handle: UInt64?
    let statistics: Statistics?

    func check() throws {
        if errno != 0 {
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno),
                          userInfo: [NSLocalizedDescriptionKey: message ?? "FVS operation failed"])
        }
    }
}

final class Bridge {
    private let session: UInt64
    private let closeLock = NSLock()
    private var closed = false

    init(source: String) throws {
        var identifier: UInt64 = 0
        if let error = source.withCString({ fvs_open(UnsafeMutablePointer(mutating: $0), &identifier) }) {
            defer { free(error) }
            let reply = try JSONDecoder().decode(Reply.self, from: Data(String(cString: error).utf8))
            try reply.check()
            throw POSIXError(.EIO)
        }
        session = identifier
    }

    deinit { try? close() }

    func close() throws {
        closeLock.lock()
        defer { closeLock.unlock() }
        if closed { return }
        let code = fvs_close(session)
        closed = true
        if code != 0 { throw NSError(domain: NSPOSIXErrorDomain, code: Int(code)) }
    }

    func call(_ op: String, path: String = "", fields: [String: Any] = [:]) throws -> Reply {
        var request = fields
        request["op"] = op
        request["path"] = path
        let data = try JSONSerialization.data(withJSONObject: request)
        guard let json = String(data: data, encoding: .utf8),
              let result = json.withCString({ fvs_call(session, UnsafeMutablePointer(mutating: $0)) }) else {
            throw POSIXError(.EIO)
        }
        defer { free(result) }
        let reply = try JSONDecoder().decode(Reply.self, from: Data(String(cString: result).utf8))
        try reply.check()
        return reply
    }

    func read(_ handle: UInt64, buffer: UnsafeMutableRawBufferPointer, length: Int, offset: Int64) throws -> Int {
        guard length >= 0 && length <= buffer.count else { throw POSIXError(.EINVAL) }
        var count: Int64 = 0
        let code = fvs_read(session, handle, buffer.baseAddress, Int64(length), offset, &count)
        if code != 0 { throw NSError(domain: NSPOSIXErrorDomain, code: Int(code)) }
        return Int(count)
    }

    func write(_ handle: UInt64, data: Data, offset: Int64) throws -> Int {
        var count: Int64 = 0
        let code = data.withUnsafeBytes { bytes in
            fvs_write(session, handle, UnsafeMutableRawPointer(mutating: bytes.baseAddress),
                      Int64(bytes.count), offset, &count)
        }
        if code != 0 { throw NSError(domain: NSPOSIXErrorDomain, code: Int(code)) }
        return Int(count)
    }
}
