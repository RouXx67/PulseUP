import { NodeConfig } from '../types/nodes';
import { apiFetchJSON } from '@/utils/apiClient';

export class NodesAPI {
  private static readonly baseUrl = '/api/config/nodes';

  static async getNodes(): Promise<NodeConfig[]> {
    // The API returns an array of nodes directly
    const nodes: NodeConfig[] = await apiFetchJSON(this.baseUrl);
    return nodes;
  }

  static async addNode(node: NodeConfig): Promise<{ success: boolean; message?: string }> {
    return apiFetchJSON(this.baseUrl, {
      method: 'POST',
      body: JSON.stringify(node),
    });
  }

  static async updateNode(
    nodeId: string,
    node: NodeConfig,
  ): Promise<{ success: boolean; message?: string }> {
    return apiFetchJSON(`${this.baseUrl}/${nodeId}`, {
      method: 'PUT',
      body: JSON.stringify(node),
    });
  }

  static async deleteNode(nodeId: string): Promise<{ success: boolean; message?: string }> {
    return apiFetchJSON(`${this.baseUrl}/${nodeId}`, {
      method: 'DELETE',
    });
  }

  static async testConnection(node: NodeConfig): Promise<{
    status: string;
    message?: string;
    isCluster?: boolean;
    nodeCount?: number;
    clusterNodeCount?: number;
    datastoreCount?: number;
  }> {
    return apiFetchJSON(`${this.baseUrl}/test-connection`, {
      method: 'POST',
      body: JSON.stringify(node),
    });
  }

  static async testExistingNode(nodeId: string): Promise<{
    status: string;
    message?: string;
    latency?: number;
  }> {
    return apiFetchJSON(`${this.baseUrl}/${nodeId}/test`, {
      method: 'POST',
    });
  }
}
