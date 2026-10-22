// ScreenCaptureKit 桥接：macOS 15 SDK 已将 CGDisplayCreateImage /
// CGDisplayCreateImageForRect / CGWindowListCreateImage 全部标记为 obsolete
// （SCREEN_CAPTURE_OBSOLETE(...,15.0)），无法再用于编译。这里改用
// ScreenCaptureKit 的 SCScreenshotManager（macOS 14+）截取整块显示器，
// 由 Go 侧把整屏图像裁剪到请求区域。
//
// 返回 +1 的 CGImageRef（调用方负责 CGImageRelease）；失败（权限拒绝、
// 无匹配显示器、超时、系统版本过低）返回 NULL。
//
// 注：dispatch_semaphore_t 在本文件的非 ARC 编译下不显式释放（每次截屏
// 泄漏一个极小的信号量对象），以换取「等待方超时后回调仍可安全 signal」的
// 简单与安全。

#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#include <dispatch/dispatch.h>

void* DSCCaptureDisplayImage(CGDirectDisplayID displayID) {
    if (@available(macOS 14.0, *)) {
        __block CGImageRef captured = NULL;
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);

        [SCShareableContent getShareableContentExcludingDesktopWindows:NO
                                                   onScreenWindowsOnly:YES
                                                     completionHandler:^(SCShareableContent *content, NSError *error) {
            if (error != nil || content == nil) {
                dispatch_semaphore_signal(sem);
                return;
            }
            SCDisplay *target = nil;
            for (SCDisplay *d in content.displays) {
                if (d.displayID == displayID) {
                    target = d;
                    break;
                }
            }
            if (target == nil) {
                dispatch_semaphore_signal(sem);
                return;
            }

            SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:target excludingWindows:@[]];
            SCStreamConfiguration *cfg = [[SCStreamConfiguration alloc] init];
            cfg.width = target.width;
            cfg.height = target.height;
            cfg.showsCursor = NO;

            [SCScreenshotManager captureImageWithFilter:filter
                                          configuration:cfg
                                      completionHandler:^(CGImageRef image, NSError *err) {
                if (image != NULL) {
                    captured = CGImageRetain(image);
                }
                dispatch_semaphore_signal(sem);
            }];
            // 异步接口会 retain 参数直至完成；这里释放本函数持有的 +1 引用。
            [filter release];
            [cfg release];
        }];

        dispatch_time_t deadline = dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC);
        if (dispatch_semaphore_wait(sem, deadline) != 0) {
            return NULL;
        }
        return (void *)captured;
    }
    return NULL;
}
