"use client"

import * as React from "react"
import { XIcon } from "lucide-react"
import { Dialog as DialogPrimitive } from "radix-ui"

import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"

function isInteractivePortalTarget(target: EventTarget | null) {
  return (
    target instanceof Element &&
    Boolean(target.closest('[data-select-dropdown="true"]'))
  )
}

function Sheet({ ...props }: React.ComponentProps<typeof DialogPrimitive.Root>) {
  return <DialogPrimitive.Root data-slot="sheet" {...props} />
}

function SheetTrigger({
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Trigger>) {
  return <DialogPrimitive.Trigger data-slot="sheet-trigger" {...props} />
}

function SheetClose({
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Close>) {
  return <DialogPrimitive.Close data-slot="sheet-close" {...props} />
}

function SheetPortal({
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Portal>) {
  return <DialogPrimitive.Portal data-slot="sheet-portal" {...props} />
}

function SheetOverlay({
  className,
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Overlay>) {
  return (
    <DialogPrimitive.Overlay
      data-slot="sheet-overlay"
      className={cn(
        "fixed inset-0 z-50 bg-black/40 data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:animate-in data-[state=open]:fade-in-0",
        className,
      )}
      {...props}
    />
  )
}

function SheetContent({
  className,
  children,
  side = "right",
  showCloseButton = true,
  onInteractOutside,
  onPointerDownOutside,
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Content> & {
  side?: "right" | "left"
  showCloseButton?: boolean
}) {
  return (
    <SheetPortal>
      <SheetOverlay />
      <DialogPrimitive.Content
        data-slot="sheet-content"
        className={cn(
          // Floating inset panel: breathing room from viewport edges
          // instead of full-bleed against browser chrome.
          "fixed z-50 flex h-auto w-[min(calc(100%-1.5rem),440px)] max-w-[min(calc(100%-1.5rem),440px)] flex-col gap-0 overflow-hidden rounded-2xl border bg-background shadow-2xl outline-none duration-200 data-[state=closed]:animate-out data-[state=open]:animate-in sm:w-[min(calc(100%-2rem),440px)] sm:max-w-[min(calc(100%-2rem),440px)]",
          "top-[max(0.75rem,env(safe-area-inset-top))] bottom-[max(0.75rem,env(safe-area-inset-bottom))] max-h-[calc(100dvh-1.5rem-env(safe-area-inset-top)-env(safe-area-inset-bottom))] sm:top-[max(1rem,env(safe-area-inset-top))] sm:bottom-[max(1rem,env(safe-area-inset-bottom))] sm:max-h-[calc(100dvh-2rem-env(safe-area-inset-top)-env(safe-area-inset-bottom))]",
          side === "right" &&
            "right-[max(0.75rem,env(safe-area-inset-right))] sm:right-[max(1rem,env(safe-area-inset-right))] data-[state=closed]:slide-out-to-right data-[state=open]:slide-in-from-right",
          side === "left" &&
            "left-[max(0.75rem,env(safe-area-inset-left))] sm:left-[max(1rem,env(safe-area-inset-left))] data-[state=closed]:slide-out-to-left data-[state=open]:slide-in-from-left",
          className,
        )}
        onInteractOutside={(event) => {
          onInteractOutside?.(event)
          if (isInteractivePortalTarget(event.target)) {
            event.preventDefault()
          }
        }}
        onPointerDownOutside={(event) => {
          onPointerDownOutside?.(event)
          if (isInteractivePortalTarget(event.target)) {
            event.preventDefault()
          }
        }}
        {...props}
      >
        {children}
        {showCloseButton ? (
          <DialogPrimitive.Close
            data-slot="sheet-close"
            className="absolute top-3.5 right-3.5 rounded-md p-1.5 text-muted-foreground opacity-80 ring-offset-background transition-opacity hover:bg-muted hover:opacity-100 focus:ring-2 focus:ring-ring focus:ring-offset-2 focus:outline-hidden disabled:pointer-events-none [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4"
          >
            <XIcon />
            <span className="sr-only">Close</span>
          </DialogPrimitive.Close>
        ) : null}
      </DialogPrimitive.Content>
    </SheetPortal>
  )
}

function SheetHeader({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="sheet-header"
      className={cn(
        "flex shrink-0 flex-col gap-1.5 border-b border-border px-5 py-4 pr-12",
        className,
      )}
      {...props}
    />
  )
}

function SheetFooter({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="sheet-footer"
      className={cn(
        // Extra bottom padding so action grids don't sit flush against the card edge.
        "flex shrink-0 flex-col gap-2 border-t border-border px-5 pt-4 pb-[max(1.25rem,calc(0.75rem+env(safe-area-inset-bottom)))] sm:pt-4 sm:pb-[max(1.5rem,calc(1rem+env(safe-area-inset-bottom)))]",
        className,
      )}
      {...props}
    />
  )
}

function SheetTitle({
  className,
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Title>) {
  return (
    <DialogPrimitive.Title
      data-slot="sheet-title"
      className={cn("text-base font-semibold leading-snug text-foreground", className)}
      {...props}
    />
  )
}

function SheetDescription({
  className,
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Description>) {
  return (
    <DialogPrimitive.Description
      data-slot="sheet-description"
      className={cn("text-sm text-muted-foreground", className)}
      {...props}
    />
  )
}

function SheetBody({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="sheet-body"
      className={cn("min-h-0 flex-1 overflow-y-auto px-5 py-4", className)}
      {...props}
    />
  )
}

export {
  Sheet,
  SheetBody,
  SheetClose,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetOverlay,
  SheetPortal,
  SheetTitle,
  SheetTrigger,
  Button as SheetActionButton,
}
