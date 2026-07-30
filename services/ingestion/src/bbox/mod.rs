// services/ingestion/src/bbox/mod.rs
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq)]
pub struct BoundingBox {
    pub x: f32,      // 0.0 - 1.0 (left)
    pub y: f32,      // 0.0 - 1.0 (top)
    pub width: f32,  // 0.0 - 1.0
    pub height: f32, // 0.0 - 1.0
}

impl BoundingBox {
    pub fn new(x: f32, y: f32, width: f32, height: f32) -> Self {
        Self {
            x: x.clamp(0.0, 1.0),
            y: y.clamp(0.0, 1.0),
            width: width.clamp(0.0, 1.0),
            height: height.clamp(0.0, 1.0),
        }
    }

    pub fn from_pixel_coords(
        x: f32, y: f32, width: f32, height: f32,
        page_width: f32, page_height: f32
    ) -> Self {
        Self {
            x: (x / page_width).clamp(0.0, 1.0),
            y: (y / page_height).clamp(0.0, 1.0),
            width: (width / page_width).clamp(0.0, 1.0),
            height: (height / page_height).clamp(0.0, 1.0),
        }
    }

    pub fn to_pixel_coords(&self, page_width: f32, page_height: f32) -> (f32, f32, f32, f32) {
        (
            self.x * page_width,
            self.y * page_height,
            self.width * page_width,
            self.height * page_height,
        )
    }

    pub fn contains(&self, other: &BoundingBox) -> bool {
        other.x >= self.x &&
        other.y >= self.y &&
        other.x + other.width <= self.x + self.width &&
        other.y + other.height <= self.y + self.height
    }

    pub fn intersects(&self, other: &BoundingBox) -> bool {
        self.x < other.x + other.width &&
        self.x + self.width > other.x &&
        self.y < other.y + other.height &&
        self.y + self.height > other.y
    }

    pub fn intersection_area(&self, other: &BoundingBox) -> f32 {
        let x_overlap = (self.x + self.width).min(other.x + other.width) - self.x.max(other.x);
        let y_overlap = (self.y + self.height).min(other.y + other.height) - self.y.max(other.y);
        if x_overlap > 0.0 && y_overlap > 0.0 {
            x_overlap * y_overlap
        } else {
            0.0
        }
    }

    pub fn iou(&self, other: &BoundingBox) -> f32 {
        let intersection = self.intersection_area(other);
        let area1 = self.width * self.height;
        let area2 = other.width * other.height;
        let union = area1 + area2 - intersection;
        if union > 0.0 {
            intersection / union
        } else {
            0.0
        }
    }

    pub fn center(&self) -> (f32, f32) {
        (self.x + self.width / 2.0, self.y + self.height / 2.0)
    }

    pub fn expand(&self, factor: f32) -> Self {
        let cx = self.x + self.width / 2.0;
        let cy = self.y + self.height / 2.0;
        let new_w = self.width * factor;
        let new_h = self.height * factor;
        Self::new(
            cx - new_w / 2.0,
            cy - new_h / 2.0,
            new_w,
            new_h,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_bbox_normalization() {
        let bbox = BoundingBox::from_pixel_coords(100.0, 200.0, 50.0, 30.0, 1000.0, 2000.0);
        assert_eq!(bbox.x, 0.1);
        assert_eq!(bbox.y, 0.1);
        assert_eq!(bbox.width, 0.05);
        assert_eq!(bbox.height, 0.015);
    }

    #[test]
    fn test_bbox_clamping() {
        let bbox = BoundingBox::new(-0.1, 1.2, 0.5, 0.5);
        assert_eq!(bbox.x, 0.0);
        assert_eq!(bbox.y, 1.0);
        assert_eq!(bbox.width, 0.5);
        assert_eq!(bbox.height, 0.5);
    }

    #[test]
    fn test_iou_identical() {
        let a = BoundingBox::new(0.1, 0.1, 0.2, 0.2);
        let b = BoundingBox::new(0.1, 0.1, 0.2, 0.2);
        assert_eq!(a.iou(&b), 1.0);
    }

    #[test]
    fn test_iou_no_overlap() {
        let a = BoundingBox::new(0.1, 0.1, 0.2, 0.2);
        let b = BoundingBox::new(0.5, 0.5, 0.2, 0.2);
        assert_eq!(a.iou(&b), 0.0);
    }

    #[test]
    fn test_contains() {
        let outer = BoundingBox::new(0.0, 0.0, 1.0, 1.0);
        let inner = BoundingBox::new(0.2, 0.2, 0.3, 0.3);
        assert!(outer.contains(&inner));
        assert!(!inner.contains(&outer));
    }
}