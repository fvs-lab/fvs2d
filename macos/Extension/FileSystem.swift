import Foundation
import FSKit

@main
struct FVSExtension: UnaryFileSystemExtension {
    typealias FileSystem = FSUnaryFileSystem & FSUnaryFileSystemOperations

    var fileSystem: FSUnaryFileSystem & FSUnaryFileSystemOperations { FVSFileSystem() }
}

final class FVSFileSystem: FSUnaryFileSystem, FSUnaryFileSystemOperations {
    private var resource: FSPathURLResource?
    private var volume: FVSVolume?
    private let resourceLock = NSLock()

    func probeResource(resource: FSResource, replyHandler: @escaping (FSProbeResult?, Error?) -> Void) {
        guard resource is FSPathURLResource else { return replyHandler(nil, POSIXError(.ENODEV)) }
        replyHandler(.usable(name: "FVS", containerID: FSContainerIdentifier(uuid: UUID())), nil)
    }

    func loadResource(resource: FSResource, options: FSTaskOptions,
                      replyHandler: @escaping (FSVolume?, Error?) -> Void) {
        resourceLock.lock()
        defer { resourceLock.unlock() }
        guard self.resource == nil else { return replyHandler(nil, POSIXError(.EBUSY)) }
        guard let pathResource = resource as? FSPathURLResource else {
            return replyHandler(nil, POSIXError(.EINVAL))
        }
        if options.taskOptions.contains(where: { $0 == "-f" }) {
            return replyHandler(nil, POSIXError(.ENOTSUP))
        }
        guard pathResource.url.startAccessingSecurityScopedResource() else {
            return replyHandler(nil, POSIXError(.EACCES))
        }
        do {
            let volume = try FVSVolume(source: pathResource.url.path)
            self.resource = pathResource
            self.volume = volume
            containerStatus = .ready
            replyHandler(volume, nil)
        } catch {
            pathResource.url.stopAccessingSecurityScopedResource()
            replyHandler(nil, error)
        }
    }

    func unloadResource(resource: FSResource, options: FSTaskOptions,
                        replyHandler: @escaping (Error?) -> Void) {
        resourceLock.lock()
        defer { resourceLock.unlock() }
        guard let pathResource = resource as? FSPathURLResource,
              let loaded = self.resource, loaded.url == pathResource.url else {
            return replyHandler(POSIXError(.EINVAL))
        }
        var failure: Error?
        do { try volume?.shutdown() } catch { failure = error }
        volume = nil
        loaded.url.stopAccessingSecurityScopedResource()
        self.resource = nil
        replyHandler(failure)
    }
}
