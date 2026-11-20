import { Navigate, createBrowserRouter } from 'react-router-dom'

import { ConsoleLayout } from '@/components/layout/ConsoleLayout'
import { RequireAuth } from '@/features/auth/RequireAuth'
import { LoginPage } from '@/features/auth/LoginPage'
import { SetupPage } from '@/features/auth/SetupPage'
import { DashboardPage } from '@/features/dashboard/DashboardPage'
import { ImagesListPage } from '@/features/images/ImagesListPage'
import { ImageDetailPage } from '@/features/images/ImageDetailPage'
import { ImportImagePage } from '@/features/images/ImportImagePage'
import { InstancesListPage } from '@/features/instances/InstancesListPage'
import { LaunchInstancePage } from '@/features/instances/LaunchInstancePage'
import { InstanceDetailPage } from '@/features/instances/InstanceDetailPage'
import { InstanceTypesListPage } from '@/features/instancetypes/InstanceTypesListPage'
import {
  CreateInstanceTypePage,
  EditInstanceTypePage,
} from '@/features/instancetypes/InstanceTypeFormPage'
import { KeyPairsListPage } from '@/features/keypairs/KeyPairsListPage'
import { CreateKeyPairPage } from '@/features/keypairs/CreateKeyPairPage'
import { ImportKeyPairPage } from '@/features/keypairs/ImportKeyPairPage'
import { VpcsListPage } from '@/features/vpcs/VpcsListPage'
import { VpcDetailPage } from '@/features/vpcs/VpcDetailPage'
import { CreateVpcPage } from '@/features/vpcs/CreateVpcPage'
import { SubnetsListPage } from '@/features/subnets/SubnetsListPage'
import { SubnetDetailPage } from '@/features/subnets/SubnetDetailPage'
import { CreateSubnetPage } from '@/features/subnets/CreateSubnetPage'
import { NotFoundPage } from '@/features/system/NotFoundPage'
import { BucketsListPage } from '@/features/buckets/BucketsListPage'
import { CreateBucketPage } from '@/features/buckets/CreateBucketPage'
import { BucketDetailPage } from '@/features/buckets/BucketDetailPage'
import { DBInstancesListPage } from '@/features/dbinstances/DBInstancesListPage'
import { CreateDBInstancePage } from '@/features/dbinstances/CreateDBInstancePage'
import { DBInstanceDetailPage } from '@/features/dbinstances/DBInstanceDetailPage'
import { ClustersListPage } from '@/features/kubernetes/ClustersListPage'
import { CreateClusterPage } from '@/features/kubernetes/CreateClusterPage'
import { ClusterDetailPage } from '@/features/kubernetes/ClusterDetailPage'
import { AccessKeysListPage } from '@/features/accesskeys/AccessKeysListPage'
import { CreateAccessKeyPage, EditAccessKeyPage } from '@/features/accesskeys/AccessKeyFormPage'

/**
 * Console routes from 03-frontend.md §3. `/login` and `/setup` are public, and
 * every console route sits behind `RequireAuth`, which redirects to
 * `/login?next=…` while the session is anonymous.
 */
export const router = createBrowserRouter([
  { path: '/login', element: <LoginPage /> },
  { path: '/setup', element: <SetupPage /> },
  {
    path: '/',
    element: (
      <RequireAuth>
        <ConsoleLayout />
      </RequireAuth>
    ),
    children: [
      { index: true, element: <DashboardPage /> },
      { path: 'instances', element: <InstancesListPage /> },
      { path: 'instances/launch', element: <LaunchInstancePage /> },
      { path: 'instances/:instanceId', element: <InstanceDetailPage /> },
      { path: 'instance-types', element: <InstanceTypesListPage /> },
      { path: 'instance-types/create', element: <CreateInstanceTypePage /> },
      { path: 'instance-types/:instanceTypeId/edit', element: <EditInstanceTypePage /> },
      { path: 'images', element: <ImagesListPage /> },
      { path: 'images/import', element: <ImportImagePage /> },
      { path: 'images/:imageId', element: <ImageDetailPage /> },
      { path: 'key-pairs', element: <KeyPairsListPage /> },
      { path: 'key-pairs/create', element: <CreateKeyPairPage /> },
      { path: 'key-pairs/import', element: <ImportKeyPairPage /> },
      { path: 'vpcs', element: <VpcsListPage /> },
      { path: 'vpcs/create', element: <CreateVpcPage /> },
      { path: 'vpcs/:vpcId', element: <VpcDetailPage /> },
      { path: 'subnets', element: <SubnetsListPage /> },
      { path: 'subnets/create', element: <CreateSubnetPage /> },
      { path: 'subnets/:subnetId', element: <SubnetDetailPage /> },
      { path: 'buckets', element: <BucketsListPage /> },
      { path: 'buckets/create', element: <CreateBucketPage /> },
      { path: 'buckets/:bucketName', element: <BucketDetailPage /> },
      { path: 'databases', element: <DBInstancesListPage /> },
      { path: 'databases/create', element: <CreateDBInstancePage /> },
      { path: 'databases/:dbInstanceId', element: <DBInstanceDetailPage /> },
      { path: 'kubernetes', element: <ClustersListPage /> },
      { path: 'kubernetes/create', element: <CreateClusterPage /> },
      { path: 'kubernetes/:clusterId', element: <ClusterDetailPage /> },
      { path: 'access-keys', element: <AccessKeysListPage /> },
      { path: 'access-keys/create', element: <CreateAccessKeyPage /> },
      { path: 'access-keys/:accessKeyId/edit', element: <EditAccessKeyPage /> },
      { path: '*', element: <NotFoundPage /> },
    ],
  },
  {
    // A stray deep link should land somewhere useful, not on the dashboard.
    path: '*',
    element: <Navigate replace to="/" />,
  },
])
