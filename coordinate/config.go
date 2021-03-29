package coordinate

// Config is used to set the parameters of the Vivaldi-based coordinate mapping
// algorithm.
//
// The following references are called out at various points in the documentation
// here:
//
// [1] Dabek, Frank, et al. "Vivaldi: A decentralized network coordinate system."
//     ACM SIGCOMM Computer Communication Review. Vol. 34. No. 4. ACM, 2004.
// [2] Ledlie, Jonathan, Paul Gardner, and Margo I. Seltzer. "Network Coordinates
//     in the Wild." NSDI. Vol. 7. 2007.
// [3] Lee, Sanghwan, et al. "On suitability of Euclidean embedding for
//     host-based network coordinate systems." Networking, IEEE/ACM Transactions
//     on 18.1 (2010): 27-40.
type Config struct {
	// The dimensionality of the coordinate system. As discussed in [2], more
	// dimensions improves the accuracy of the estimates up to a point. Per [2]
	// we chose 8 dimensions plus a non-Euclidean height.
	// 坐标系统的维数。正如在[2]中讨论的，更多的维度在一定程度上提高了估计的准确性。每个[2]我们选择了8个维度加上一个非欧几里得高度。
	Dimensionality uint

	// VivaldiErrorMax is the default error value when a node hasn't yet made
	// any observations. It also serves as an upper limit on the error value in
	// case observations cause the error value to increase without bound.
	// VivaldiErrorMax是节点尚未进行任何观察时的默认错误值。它还可以作为误差值的上限，以防观测结果导致误差值无限制地增加。
	VivaldiErrorMax float64

	// VivaldiCE is a tuning factor that controls the maximum impact an
	// observation can have on a node's confidence. See [1] for more details.
	// VivaldiCE是一个调优因子，它控制观察结果对节点置信度的最大影响。请参阅[1]了解更多细节。
	VivaldiCE float64

	// VivaldiCC is a tuning factor that controls the maximum impact an
	// observation can have on a node's coordinate. See [1] for more details.
	// VivaldiCC是一个调优因子，它控制观测对节点坐标的最大影响。请参阅[1]了解更多细节。
	VivaldiCC float64

	// AdjustmentWindowSize is a tuning factor that determines how many samples
	// we retain to calculate the adjustment factor as discussed in [3]. Setting
	// this to zero disables this feature.
	// AdjustmentWindowSize是一个调整因子，它决定了我们保留多少样本来计算在[3]中讨论的调整因子。将此设置为0将禁用此特性。
	AdjustmentWindowSize uint

	// HeightMin is the minimum value of the height parameter. Since this
	// always must be positive, it will introduce a small amount error, so
	// the chosen value should be relatively small compared to "normal"
	// coordinates.
	// HeightMin是高度参数的最小值。因为这个值必须总是正的，所以它会引入一个小的误差，所以所选择的值与“正常”坐标相比应该是相对较小的。
	HeightMin float64

	// LatencyFilterSamples is the maximum number of samples that are retained
	// per node, in order to compute a median. The intent is to ride out blips
	// but still keep the delay low, since our time to probe any given node is
	// pretty infrequent. See [2] for more details.
	// LatencyFilterSamples是每个节点保留的最大样本数，以便计算中值。这样做的目的是避免出现故障，但仍然保持较低的延迟，因为探测任何给定节点的时间非常少。请参阅[2]了解更多细节。
	LatencyFilterSize uint

	// GravityRho is a tuning factor that sets how much gravity has an effect
	// to try to re-center coordinates. See [2] for more details.
	// GravityRho是一个调整因子，用于设置重力对重新调整坐标中心的影响。请参阅[2]了解更多细节。
	GravityRho float64
}

// DefaultConfig returns a Config that has some default values suitable for
// basic testing of the algorithm, but not tuned to any particular type of cluster.
func DefaultConfig() *Config {
	return &Config{
		Dimensionality:       8,
		VivaldiErrorMax:      1.5,
		VivaldiCE:            0.25,
		VivaldiCC:            0.25,
		AdjustmentWindowSize: 20,
		HeightMin:            10.0e-6,
		LatencyFilterSize:    3,
		GravityRho:           150.0,
	}
}
